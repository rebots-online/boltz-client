package nursery

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/BoltzExchange/boltz-lnd/boltz"
	"github.com/BoltzExchange/boltz-lnd/database"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/google/logger"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/r3labs/sse"
	"strconv"
	"sync"
)

// Map between Swap ids and a channel that tells its SSE event listeners to stop
var eventListeners = make(map[string]chan bool)
var eventListenersLock sync.RWMutex

// TODO: abstract interactions with chain (querying and broadcasting transactions) into interface to be able to switch between Boltz API and bitcoin core
func (nursery *Nursery) startBlockListener(blockNotifier chan *chainrpc.BlockEpoch) {
	go func() {
		for {
			newBlock := <-blockNotifier

			swapsToRefund, err := nursery.database.QueryRefundableSwaps(newBlock.Height)

			if err != nil {
				logger.Error("Could not query refundable Swaps: " + err.Error())
				continue
			}

			if len(swapsToRefund) > 0 {
				logger.Info("Found " + strconv.Itoa(len(swapsToRefund)) + " Swaps to refund at height " + strconv.FormatUint(uint64(newBlock.Height), 10))

				for _, swapToRefund := range swapsToRefund {
					eventListenersLock.RLock()
					stopListening, hasListener := eventListeners[swapToRefund.Id]
					eventListenersLock.RUnlock()

					if hasListener {
						stopListening <- true

						eventListenersLock.Lock()
						delete(eventListeners, swapToRefund.Id)
						eventListenersLock.Unlock()
					}

					nursery.refundSwap(swapToRefund)
				}
			}
		}
	}()
}

func (nursery *Nursery) refundSwap(swap database.Swap) {
	addressString, err := nursery.lnd.NewAddress()

	if err != nil {
		logger.Error("Could not get new address from LND: " + err.Error())
		return
	}

	address, err := btcutil.DecodeAddress(addressString, nursery.chainParams)

	if err != nil {
		logger.Error("Could not decode destination address from LND: " + err.Error())
		return
	}

	logger.Info("Refunding Swap " + swap.Id + " to: " + addressString)

	swapTransactionResponse, err := nursery.boltz.GetSwapTransaction(swap.Id)

	if err != nil {
		logger.Error("Could not get lockup transaction from Boltz: " + err.Error())
		return
	}

	lockupTransactionRaw, err := hex.DecodeString(swapTransactionResponse.TransactionHex)

	if err != nil {
		logger.Error("Could not decode lockup transaction from Boltz: " + err.Error())
		return
	}

	lockupTransaction, err := btcutil.NewTxFromBytes(lockupTransactionRaw)

	if err != nil {
		logger.Error("Could not parse lockup transaction from Boltz: " + err.Error())
		return
	}

	logger.Info("Got lockup transaction of Swap " + swap.Id + " from Boltz: " + lockupTransaction.Hash().String())
	lockupVout, err := nursery.findLockupVout(swap.Address, lockupTransaction.MsgTx().TxOut)

	if err != nil {
		logger.Error("Could not find lockup vout of Swap " + swap.Id)
		return
	}

	refundTransaction, err := boltz.ConstructRefundTransaction(
		lockupTransaction,
		lockupVout,
		swap.PrivateKey,
		swap.RedeemScript,
		address,
		uint32(swap.TimoutBlockHeight),
	)

	if err != nil {
		logger.Error("Could not construct refund transaction: " + err.Error())
		return
	}

	logger.Info("Constructed refund transaction for Swap " + swap.Id + ": " + refundTransaction.TxHash().String())
	refundTransactionHex, err := boltz.SerializeTransaction(refundTransaction)

	if err != nil {
		logger.Error("Could not serialize refund transaction: " + err.Error())
		return
	}

	_, err = nursery.boltz.BroadcastTransaction(refundTransactionHex)

	if err != nil {
		logger.Error("Could not broadcast refund transaction: " + err.Error())
		return
	}

	logger.Info("Broadcast refund transaction of Swap " + swap.Id + " with Boltz API")

	nursery.handleSwapStatus(&swap, boltz.SwapRefunded.String())
}

func (nursery *Nursery) findLockupVout(addressToFind string, outputs []*wire.TxOut) (uint32, error) {
	for vout, output := range outputs {
		_, outputAddresses, _, err := txscript.ExtractPkScriptAddrs(output.PkScript, nursery.chainParams)

		// Just ignore outputs we can't decode
		if err != nil {
			continue
		}

		for _, outputAddress := range outputAddresses {
			if outputAddress.EncodeAddress() == addressToFind {
				return uint32(vout), nil
			}
		}
	}

	return 0, errors.New("could not find lockup vout")
}

func (nursery *Nursery) recoverSwaps(blockNotifier chan *chainrpc.BlockEpoch) error {
	logger.Info("Recovering pending Swaps")

	swaps, err := nursery.database.QueryPendingSwaps()

	if err != nil {
		return err
	}

	for _, swap := range swaps {
		logger.Info("Recovering Swap " + swap.Id + " at state: " + swap.Status.String())

		// TODO: handle race condition when status is updated between the POST request and the time the streaming starts
		status, err := nursery.boltz.SwapStatus(swap.Id)

		if err != nil {
			return err
		}

		if status.Status != swap.Status.String() {
			logger.Info("Swap " + swap.Id + " status changed to: " + status.Status)
			nursery.handleSwapStatus(&swap, "transaction.claimed")

			isCompleted := false

			for _, completedStatus := range boltz.CompletedStatus {
				if swap.Status.String() == completedStatus {
					isCompleted = true
					break
				}
			}

			if !isCompleted {
				nursery.RegisterSwap(swap)
			}

			continue
		}

		logger.Info("Swap " + swap.Id + " status did not change")
		nursery.RegisterSwap(swap)
	}

	nursery.startBlockListener(blockNotifier)

	return nil
}

func (nursery *Nursery) RegisterSwap(swap database.Swap) {
	logger.Info("Listening to events of Swap " + swap.Id)

	go func() {
		stopListening := make(chan bool)

		eventListenersLock.Lock()
		eventListeners[swap.Id] = stopListening
		eventListenersLock.Unlock()

		eventStream := make(chan *sse.Event)

		// TODO: handle disconnections gracefully
		go func() {
			var err error
			_, err = nursery.boltz.StreamSwapStatus(swap.Id, eventStream)

			if err != nil {
				logger.Error("Could not listen to events of Swap " + swap.Id + ": " + err.Error())
				return
			}
		}()

		for {
			select {
			case event := <-eventStream:
				var response boltz.SwapStatusResponse
				err := json.Unmarshal(event.Data, &response)

				if err == nil {
					logger.Info("Swap " + swap.Id + " status update: " + response.Status)
					nursery.handleSwapStatus(&swap, response.Status)

					// The event listening can stop after the Swap has succeeded
					if swap.Status == boltz.TransactionClaimed {
						return
					}
				} else {
					logger.Error("Could not parse update event of Swap " + swap.Id + ": " + err.Error())
				}
				break

			case <-stopListening:
				logger.Info("Stopping event listener of Swap " + swap.Id)
				return
			}
		}
	}()
}

func (nursery *Nursery) handleSwapStatus(swap *database.Swap, status string) {
	parsedStatus := boltz.ParseEvent(status)

	switch parsedStatus {
	case boltz.TransactionClaimed:
		// Verify that the invoice was actually paid
		decodedInvoice, err := zpay32.Decode(swap.Invoice, nursery.chainParams)

		if err != nil {
			logger.Warning("Could not decode invoice: " + err.Error())
			return
		}

		invoiceInfo, err := nursery.lnd.LookupInvoice(decodedInvoice.PaymentHash[:])

		if err != nil {
			logger.Warning("Could not get invoice information from LND: " + err.Error())
			return
		}

		if invoiceInfo.State != lnrpc.Invoice_SETTLED {
			logger.Warning("Swap " + swap.Id + " was not actually settled. Refunding at block " + strconv.Itoa(swap.TimoutBlockHeight))
			return
		}

		logger.Info("Swap " + swap.Id + " succeeded")
	}

	err := nursery.database.UpdateSwapStatus(swap, parsedStatus)
	if err != nil {
		logger.Error("Could not update status of Swap " + swap.Id + ": " + err.Error())
	}
}
