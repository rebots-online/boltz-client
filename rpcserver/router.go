package rpcserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/BoltzExchange/boltz-client/build"
	"github.com/golang/protobuf/ptypes/empty"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/BoltzExchange/boltz-client/onchain/wallet"

	"github.com/BoltzExchange/boltz-client/autoswap"
	"github.com/BoltzExchange/boltz-client/boltz"
	"github.com/BoltzExchange/boltz-client/boltzrpc"
	"github.com/BoltzExchange/boltz-client/database"
	"github.com/BoltzExchange/boltz-client/lightning"
	"github.com/BoltzExchange/boltz-client/logger"
	"github.com/BoltzExchange/boltz-client/nursery"
	"github.com/BoltzExchange/boltz-client/onchain"
	"github.com/BoltzExchange/boltz-client/utils"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/zpay32"
)

const referralId = "boltz-client"

type routedBoltzServer struct {
	boltzrpc.BoltzServer

	network *boltz.Network

	onchain   *onchain.Onchain
	lightning lightning.LightningNode
	boltz     *boltz.Boltz
	nursery   *nursery.Nursery
	database  *database.Database
	swapper   *autoswap.AutoSwapper

	stop   chan bool
	locked bool
}

func handleError(err error) error {
	if err != nil {
		logger.Warn("RPC request failed: " + err.Error())
	}

	return err
}

func (server *routedBoltzServer) GetInfo(_ context.Context, _ *boltzrpc.GetInfoRequest) (*boltzrpc.GetInfoResponse, error) {

	pendingSwaps, err := server.database.QueryPendingSwaps()

	if err != nil {
		return nil, handleError(err)
	}

	var pendingSwapIds []string

	for _, pendingSwap := range pendingSwaps {
		pendingSwapIds = append(pendingSwapIds, pendingSwap.Id)
	}

	pendingReverseSwaps, err := server.database.QueryPendingReverseSwaps()

	if err != nil {
		return nil, handleError(err)
	}

	var pendingReverseSwapIds []string

	for _, pendingReverseSwap := range pendingReverseSwaps {
		pendingReverseSwapIds = append(pendingReverseSwapIds, pendingReverseSwap.Id)
	}

	blockHeights := &boltzrpc.BlockHeights{}
	blockHeights.Btc, err = server.onchain.GetBlockHeight(boltz.CurrencyBtc)
	if err != nil {
		return nil, handleError(fmt.Errorf("Failed to get block height for btc: %v", err))
	}
	liquidHeight, err := server.onchain.GetBlockHeight(boltz.CurrencyLiquid)
	if err != nil {
		logger.Infof("Failed to get block height for liquid: %v", err)
	} else {
		blockHeights.Liquid = &liquidHeight
	}

	response := &boltzrpc.GetInfoResponse{
		Version:             build.GetVersion(),
		Network:             server.network.Name,
		BlockHeights:        blockHeights,
		PendingSwaps:        pendingSwapIds,
		PendingReverseSwaps: pendingReverseSwapIds,

		Symbol:      "BTC",
		BlockHeight: blockHeights.Btc,
	}

	if server.lightning != nil {
		lightningInfo, err := server.lightning.GetInfo()
		if err != nil {
			return nil, handleError(err)
		}

		response.Node = server.lightning.Name()
		response.NodePubkey = lightningInfo.Pubkey
		//nolint:staticcheck
		response.LndPubkey = lightningInfo.Pubkey
	} else {
		response.Node = "standalone"
	}

	if server.swapper != nil {
		if server.swapper.Running() {
			response.AutoSwapStatus = "running"
		} else {
			if server.swapper.Error() != "" {
				response.AutoSwapStatus = "error"
			} else {
				response.AutoSwapStatus = "disabled"
			}
		}
	}

	return response, nil

}

func (server *routedBoltzServer) GetServiceInfo(_ context.Context, request *boltzrpc.GetServiceInfoRequest) (*boltzrpc.GetServiceInfoResponse, error) {
	fees, limits, err := server.getPairs(boltz.PairBtc)

	if err != nil {
		return nil, handleError(err)
	}

	limits.Minimal = calculateDepositLimit(limits.Minimal, fees, true)
	limits.Maximal = calculateDepositLimit(limits.Maximal, fees, false)

	return &boltzrpc.GetServiceInfoResponse{
		Fees:   fees,
		Limits: limits,
	}, nil
}

func (server *routedBoltzServer) ListSwaps(_ context.Context, request *boltzrpc.ListSwapsRequest) (*boltzrpc.ListSwapsResponse, error) {
	response := &boltzrpc.ListSwapsResponse{}

	args := database.SwapQuery{
		IsAuto: request.IsAuto,
		State:  request.State,
	}

	if request.From != nil {
		parsed := utils.ParseCurrency(request.From)
		args.From = &parsed
	}

	if request.To != nil {
		parsed := utils.ParseCurrency(request.To)
		args.To = &parsed
	}

	swaps, err := server.database.QuerySwaps(args)
	if err != nil {
		return nil, err
	}

	for _, swap := range swaps {
		response.Swaps = append(response.Swaps, serializeSwap(&swap))
	}

	// Reverse Swaps
	reverseSwaps, err := server.database.QueryReverseSwaps(args)

	if err != nil {
		return nil, err
	}

	for _, reverseSwap := range reverseSwaps {
		response.ReverseSwaps = append(response.ReverseSwaps, serializeReverseSwap(&reverseSwap))
	}

	return response, nil
}

func (server *routedBoltzServer) RefundSwap(ctx context.Context, request *boltzrpc.RefundSwapRequest) (*boltzrpc.GetSwapInfoResponse, error) {
	swap, err := server.database.QuerySwap(request.Id)
	if err != nil {
		return nil, handleError(status.Errorf(codes.NotFound, "swap not found"))
	}

	if swap.LockupTransactionId == "" || swap.RefundTransactionId != "" {
		return nil, handleError(status.Errorf(codes.FailedPrecondition, "swap can not be refunded"))
	}

	if err := boltz.ValidateAddress(server.network, request.Address, swap.Pair.From); err != nil {
		return nil, handleError(status.Errorf(codes.InvalidArgument, "invalid address"))
	}

	if err := server.database.SetSwapRefundRefundAddress(swap, request.Address); err != nil {
		return nil, handleError(err)
	}

	if err := server.nursery.RefundSwaps([]database.Swap{*swap}, true); err != nil {
		return nil, handleError(err)
	}

	return server.GetSwapInfo(ctx, &boltzrpc.GetSwapInfoRequest{Id: request.Id})
}

func (server *routedBoltzServer) GetSwapInfo(_ context.Context, request *boltzrpc.GetSwapInfoRequest) (*boltzrpc.GetSwapInfoResponse, error) {
	swap, reverseSwap, err := server.database.QueryAnySwap(request.Id)
	if err != nil {
		return nil, handleError(errors.New("could not find Swap with ID " + request.Id))
	}
	return &boltzrpc.GetSwapInfoResponse{
		Swap:        serializeSwap(swap),
		ReverseSwap: serializeReverseSwap(reverseSwap),
	}, nil
}

func (server *routedBoltzServer) GetSwapInfoStream(request *boltzrpc.GetSwapInfoRequest, stream boltzrpc.Boltz_GetSwapInfoStreamServer) error {
	var updates <-chan nursery.SwapUpdate
	var stop func()

	if request.Id == "" || request.Id == "*" {
		logger.Info("Starting global Swap info stream")
		updates, stop = server.nursery.GlobalSwapUpdates()
	} else {
		logger.Info("Starting Swap info stream for " + request.Id)
		updates, stop = server.nursery.SwapUpdates(request.Id)
		if updates == nil {
			info, err := server.GetSwapInfo(context.Background(), request)
			if err != nil {
				return handleError(err)
			}
			if err := stream.Send(info); err != nil {
				return handleError(err)
			}
			return nil
		}
	}

	for update := range updates {
		if err := stream.Send(&boltzrpc.GetSwapInfoResponse{
			Swap:        serializeSwap(update.Swap),
			ReverseSwap: serializeReverseSwap(update.ReverseSwap),
		}); err != nil {
			stop()
			return handleError(err)
		}
	}

	return nil
}

func (server *routedBoltzServer) Deposit(_ context.Context, request *boltzrpc.DepositRequest) (*boltzrpc.DepositResponse, error) {
	response, err := server.createSwap(false, &boltzrpc.CreateSwapRequest{
		Pair: &boltzrpc.Pair{
			From: boltzrpc.Currency_BTC,
			To:   boltzrpc.Currency_BTC,
		},
	})
	if err != nil {
		return nil, handleError(err)
	}

	return &boltzrpc.DepositResponse{
		Id:                 response.Id,
		Address:            response.Address,
		TimeoutBlockHeight: response.TimeoutBlockHeight,
	}, nil
}

// TODO: custom refund address
func (server *routedBoltzServer) createSwap(isAuto bool, request *boltzrpc.CreateSwapRequest) (*boltzrpc.CreateSwapResponse, error) {
	logger.Info("Creating Swap for " + strconv.FormatInt(request.Amount, 10) + " satoshis")

	privateKey, publicKey, err := newKeys()
	if err != nil {
		return nil, handleError(err)
	}

	pair := utils.ParsePair(request.Pair)

	submarinePair, err := server.GetSubmarinePair(context.Background(), request.Pair)
	if err != nil {
		return nil, err
	}

	createSwap := boltz.CreateSwapRequest{
		From:            pair.From,
		To:              pair.To,
		PairHash:        submarinePair.Hash,
		RefundPublicKey: publicKey.SerializeCompressed(),
		ReferralId:      referralId,
	}

	var preimage, preimageHash []byte
	if request.GetInvoice() != "" {
		invoice, err := zpay32.Decode(request.GetInvoice(), server.network.Btc)
		if err != nil {
			return nil, handleError(fmt.Errorf("invalid invoice: %w", err))
		}
		preimageHash = invoice.PaymentHash[:]
		createSwap.Invoice = request.GetInvoice()
	} else if server.lightning == nil {
		return nil, handleError(errors.New("invoice is required in standalone mode"))
	} else if request.Amount != 0 {

		invoice, err := server.lightning.CreateInvoice(request.Amount, nil, 0, utils.GetSwapMemo(string(pair.From)))
		if err != nil {
			return nil, handleError(err)
		}
		preimageHash = invoice.PaymentHash
		createSwap.Invoice = invoice.PaymentRequest
	} else {
		if request.SendFromInternal {
			return nil, handleError(errors.New("cannot auto send if amount is 0"))
		}
		preimage, preimageHash, err = newPreimage()
		if err != nil {
			return nil, handleError(err)
		}

		logger.Info("Creating Swap with preimage hash: " + hex.EncodeToString(preimageHash))

		createSwap.PreimageHash = preimageHash
	}

	wallet, err := server.onchain.GetWallet(request.GetWallet(), pair.From, false)
	if err != nil {
		if request.SendFromInternal {
			return nil, handleError(err)
		}
	}

	response, err := server.boltz.CreateSwap(createSwap)

	if err != nil {
		return nil, handleError(errors.New("boltz error: " + err.Error()))
	}

	swap := database.Swap{
		Id:                  response.Id,
		Pair:                pair,
		State:               boltzrpc.SwapState_PENDING,
		Error:               "",
		PrivateKey:          privateKey,
		Preimage:            preimage,
		Invoice:             createSwap.Invoice,
		Address:             response.Address,
		ExpectedAmount:      response.ExpectedAmount,
		TimoutBlockHeight:   response.TimeoutBlockHeight,
		SwapTree:            response.SwapTree.Deserialize(),
		LockupTransactionId: "",
		RefundTransactionId: "",
		RefundAddress:       request.GetRefundAddress(),
		IsAuto:              isAuto,
		ServiceFeePercent:   utils.Percentage(submarinePair.Fees.Percentage),
	}

	if request.SendFromInternal {
		swap.Wallet = wallet.Name()
	}

	swap.ClaimPubKey, err = btcec.ParsePubKey([]byte(response.ClaimPublicKey))
	if err != nil {
		return nil, handleError(err)
	}

	// for _, chanId := range request.ChanIds {
	// 	parsed, err := lightning.NewChanIdFromString(chanId)
	// 	if err != nil {
	// 		return nil, handleError(errors.New("invalid channel id: " + err.Error()))
	// 	}
	// 	swap.ChanIds = append(swap.ChanIds, parsed)
	// }

	if pair.From == boltz.CurrencyLiquid {
		swap.BlindingKey, _ = btcec.PrivKeyFromBytes(response.BlindingKey)

		if err != nil {
			return nil, handleError(err)
		}
	}

	if err := swap.InitTree(); err != nil {
		return nil, handleError(err)
	}

	if err := swap.SwapTree.Check(false, swap.TimoutBlockHeight, preimageHash); err != nil {
		return nil, handleError(err)
	}

	if err := swap.SwapTree.CheckAddress(response.Address, server.network, swap.BlindingPubKey()); err != nil {
		return nil, handleError(err)
	}

	logger.Info("Verified redeem script and address of Swap " + swap.Id)

	err = server.database.CreateSwap(swap)
	if err != nil {
		return nil, handleError(err)
	}

	blockHeight, err := server.onchain.GetBlockHeight(pair.From)
	if err != nil {
		return nil, handleError(err)
	}

	timeoutHours := boltz.BlocksToHours(response.TimeoutBlockHeight-blockHeight, pair.From)
	swapResponse := &boltzrpc.CreateSwapResponse{
		Id:                 swap.Id,
		Address:            response.Address,
		ExpectedAmount:     int64(response.ExpectedAmount),
		Bip21:              response.Bip21,
		TimeoutBlockHeight: response.TimeoutBlockHeight,
		TimeoutHours:       float32(timeoutHours),
	}

	if request.SendFromInternal {
		// TODO: custom block target?
		feeSatPerVbyte, err := server.onchain.EstimateFee(pair.From, 2)
		if err != nil {
			return nil, handleError(err)
		}
		logger.Infof("Paying swap %s with fee of %f sat/vbyte", swap.Id, feeSatPerVbyte)
		txId, err := wallet.SendToAddress(response.Address, response.ExpectedAmount, feeSatPerVbyte)
		if err != nil {
			return nil, handleError(err)
		}
		swapResponse.TxId = txId
	}

	logger.Info("Created new Swap " + swap.Id + ": " + marshalJson(swap.Serialize()))

	if err := server.nursery.RegisterSwap(swap); err != nil {
		return nil, handleError(err)
	}

	return swapResponse, nil
}

func (server *routedBoltzServer) CreateSwap(_ context.Context, request *boltzrpc.CreateSwapRequest) (*boltzrpc.CreateSwapResponse, error) {
	return server.createSwap(false, request)
}

func (server *routedBoltzServer) createReverseSwap(isAuto bool, request *boltzrpc.CreateReverseSwapRequest) (*boltzrpc.CreateReverseSwapResponse, error) {
	logger.Info("Creating Reverse Swap for " + strconv.FormatInt(request.Amount, 10) + " satoshis")

	externalPay := request.GetExternalPay()
	if server.lightning == nil {
		if request.ExternalPay == nil {
			externalPay = true
		} else if !externalPay {
			return nil, handleError(errors.New("can not create reverse swap without external pay in standalone mode"))
		}
	}

	returnImmediately := request.GetReturnImmediately()
	if externalPay {
		// only error if it was explicitly set to false, implicitly set to true otherwise
		if request.ReturnImmediately != nil && !returnImmediately {
			return nil, handleError(errors.New("can not wait for swap transaction when using external pay"))
		} else {
			returnImmediately = true
		}
	}

	claimAddress := request.Address

	pair := utils.ParsePair(request.Pair)
	if claimAddress != "" {
		err := boltz.ValidateAddress(server.network, claimAddress, pair.To)

		if err != nil {
			return nil, handleError(fmt.Errorf("Invalid claim address %s: %w", claimAddress, err))
		}
	} else {
		wallet, err := server.onchain.GetWallet(request.GetWallet(), pair.To, true)
		if err != nil {
			return nil, handleError(err)
		}

		claimAddress, err = wallet.NewAddress()
		if err != nil {
			return nil, handleError(err)
		}

		logger.Infof("Got claim address from wallet %v: %v", wallet.Name(), claimAddress)
	}

	preimage, preimageHash, err := newPreimage()

	if err != nil {
		return nil, handleError(err)
	}

	logger.Info("Generated preimage " + hex.EncodeToString(preimage))

	privateKey, publicKey, err := newKeys()

	if err != nil {
		return nil, handleError(err)
	}

	reversePair, err := server.GetReversePair(context.Background(), request.Pair)
	if err != nil {
		return nil, handleError(err)
	}

	response, err := server.boltz.CreateReverseSwap(boltz.CreateReverseSwapRequest{
		From:           pair.From,
		To:             pair.To,
		PairHash:       reversePair.Hash,
		InvoiceAmount:  uint64(request.Amount),
		PreimageHash:   preimageHash,
		ClaimPublicKey: publicKey.SerializeCompressed(),
		ReferralId:     referralId,
	})
	if err != nil {
		return nil, handleError(err)
	}

	key, err := btcec.ParsePubKey(response.RefundPublicKey)
	if err != nil {
		return nil, handleError(err)
	}

	reverseSwap := database.ReverseSwap{
		Id:                  response.Id,
		IsAuto:              isAuto,
		Pair:                pair,
		Status:              boltz.SwapCreated,
		AcceptZeroConf:      request.AcceptZeroConf,
		PrivateKey:          privateKey,
		SwapTree:            response.SwapTree.Deserialize(),
		RefundPubKey:        key,
		Preimage:            preimage,
		Invoice:             response.Invoice,
		ClaimAddress:        claimAddress,
		OnchainAmount:       response.OnchainAmount,
		TimeoutBlockHeight:  response.TimeoutBlockHeight,
		LockupTransactionId: "",
		ClaimTransactionId:  "",
		ServiceFeePercent:   utils.Percentage(reversePair.Fees.Percentage),
		ExternalPay:         externalPay,
	}

	for _, chanId := range request.ChanIds {
		parsed, err := lightning.NewChanIdFromString(chanId)
		if err != nil {
			return nil, handleError(errors.New("invalid channel id: " + err.Error()))
		}
		reverseSwap.ChanIds = append(reverseSwap.ChanIds, parsed)
	}

	var blindingPubKey *btcec.PublicKey
	if reverseSwap.Pair.To == boltz.CurrencyLiquid {
		reverseSwap.BlindingKey, _ = btcec.PrivKeyFromBytes(response.BlindingKey)
		blindingPubKey = reverseSwap.BlindingKey.PubKey()

		if err != nil {
			return nil, handleError(err)
		}
	}

	if err := reverseSwap.InitTree(); err != nil {
		return nil, handleError(err)
	}

	if err := reverseSwap.SwapTree.Check(true, reverseSwap.TimeoutBlockHeight, preimageHash); err != nil {
		return nil, handleError(err)
	}

	if err := reverseSwap.SwapTree.CheckAddress(response.LockupAddress, server.network, blindingPubKey); err != nil {
		return nil, handleError(err)
	}

	invoice, err := zpay32.Decode(reverseSwap.Invoice, server.network.Btc)

	if err != nil {
		return nil, handleError(err)
	}

	if !bytes.Equal(preimageHash, invoice.PaymentHash[:]) {
		return nil, handleError(errors.New("invalid invoice preimage hash"))
	}

	logger.Info("Verified redeem script and invoice of Reverse Swap " + reverseSwap.Id)

	err = server.database.CreateReverseSwap(reverseSwap)

	if err != nil {
		return nil, handleError(err)
	}

	if err := server.nursery.RegisterReverseSwap(reverseSwap); err != nil {
		return nil, handleError(err)
	}

	logger.Info("Created new Reverse Swap " + reverseSwap.Id + ": " + marshalJson(reverseSwap.Serialize()))

	rpcResponse := &boltzrpc.CreateReverseSwapResponse{
		Id:            reverseSwap.Id,
		LockupAddress: response.LockupAddress,
	}

	if externalPay {
		rpcResponse.Invoice = &reverseSwap.Invoice
	} else {
		if err := server.nursery.PayReverseSwap(&reverseSwap); err != nil {
			if dbErr := server.database.UpdateReverseSwapState(&reverseSwap, boltzrpc.SwapState_ERROR, err.Error()); dbErr != nil {
				return nil, handleError(dbErr)
			}
			return nil, handleError(err)
		}
	}

	if !returnImmediately && request.AcceptZeroConf {
		updates, stop := server.nursery.SwapUpdates(reverseSwap.Id)
		defer stop()

		for update := range updates {
			info := update.ReverseSwap
			if info.State == boltzrpc.SwapState_SUCCESSFUL {
				rpcResponse.ClaimTransactionId = &update.ReverseSwap.ClaimTransactionId
				rpcResponse.RoutingFeeMilliSat = update.ReverseSwap.RoutingFeeMsat
			}
			if info.State == boltzrpc.SwapState_ERROR || info.State == boltzrpc.SwapState_SERVER_ERROR {
				return nil, handleError(errors.New("reverse swap failed: " + info.Error))
			}
		}

	}

	return rpcResponse, nil
}

func (server *routedBoltzServer) CreateReverseSwap(_ context.Context, request *boltzrpc.CreateReverseSwapRequest) (*boltzrpc.CreateReverseSwapResponse, error) {
	return server.createReverseSwap(false, request)
}

func (server *routedBoltzServer) importWallet(credentials *wallet.Credentials, password string) error {
	decryptWalletCredentials, err := server.decryptWalletCredentials(password)
	if err != nil {
		return errors.New("wrong password")
	}

	for _, existing := range decryptWalletCredentials {
		if existing.Mnemonic == credentials.Mnemonic && existing.Xpub == credentials.Xpub && existing.CoreDescriptor == credentials.CoreDescriptor {
			return fmt.Errorf("wallet %s has the same credentials", existing.Name)
		}
	}

	wallet, err := wallet.Login(credentials)
	if err != nil {
		return errors.New("could not login: " + err.Error())
	}
	if wallet.Readonly() {
		var subaccount *uint64
		subaccounts, err := wallet.GetSubaccounts(false)
		if err != nil {
			return err
		}
		if len(subaccounts) != 0 {
			subaccount = &subaccounts[0].Pointer
		}
		credentials.Subaccount, err = wallet.SetSubaccount(subaccount)
		if err != nil {
			return err
		}
	}
	decryptWalletCredentials = append(decryptWalletCredentials, credentials)
	if err := server.database.InsertWalletCredentials(credentials); err != nil {
		return err
	}
	if password != "" {
		if err := server.encryptWalletCredentials(password, decryptWalletCredentials); err != nil {
			return fmt.Errorf("could not encrypt credentials: %w", err)
		}
	}
	server.onchain.AddWallet(wallet)
	return nil
}

func (server *routedBoltzServer) ImportWallet(context context.Context, request *boltzrpc.ImportWalletRequest) (*boltzrpc.Wallet, error) {
	if err := checkName(request.Info.Name); err != nil {
		return nil, handleError(err)
	}

	currency := utils.ParseCurrency(&request.Info.Currency)
	credentials := &wallet.Credentials{
		Name:           request.Info.Name,
		Currency:       currency,
		Mnemonic:       request.Credentials.GetMnemonic(),
		Xpub:           request.Credentials.GetXpub(),
		CoreDescriptor: request.Credentials.GetCoreDescriptor(),
		Subaccount:     request.Credentials.Subaccount,
	}

	if err := server.importWallet(credentials, request.GetPassword()); err != nil {
		return nil, handleError(err)
	}
	return server.GetWallet(context, &boltzrpc.GetWalletRequest{Name: request.Info.Name})
}

func (server *routedBoltzServer) SetSubaccount(_ context.Context, request *boltzrpc.SetSubaccountRequest) (*boltzrpc.Subaccount, error) {
	wallet, err := server.getOwnWallet(request.Name, false)
	if err != nil {
		return nil, handleError(err)
	}

	subaccountNumber, err := wallet.SetSubaccount(request.Subaccount)
	if err != nil {
		return nil, handleError(err)
	}

	if err := server.database.SetWalletSubaccount(wallet.Name(), string(wallet.Currency()), *subaccountNumber); err != nil {
		return nil, handleError(err)
	}

	subaccount, err := wallet.GetSubaccount(*subaccountNumber)
	if err != nil {
		return nil, handleError(err)
	}
	balance, err := wallet.GetBalance()
	if err != nil {
		return nil, handleError(err)
	}
	return serializewalletSubaccount(*subaccount, balance), nil
}

func (server *routedBoltzServer) GetSubaccounts(_ context.Context, request *boltzrpc.WalletInfo) (*boltzrpc.GetSubaccountsResponse, error) {
	wallet, err := server.getOwnWallet(request.Name, false)
	if err != nil {
		return nil, handleError(err)
	}

	subaccounts, err := wallet.GetSubaccounts(true)
	if err != nil {
		return nil, handleError(err)
	}

	response := &boltzrpc.GetSubaccountsResponse{}
	for _, subaccount := range subaccounts {
		balance, err := wallet.GetSubaccountBalance(subaccount.Pointer)
		if err != nil {
			logger.Errorf("failed to get balance for subaccount %+v: %v", subaccount, err.Error())
		}
		response.Subaccounts = append(response.Subaccounts, serializewalletSubaccount(*subaccount, balance))
	}

	if subaccount, err := wallet.CurrentSubaccount(); err == nil {
		response.Current = &subaccount
	}
	return response, nil
}

func (server *routedBoltzServer) CreateWallet(ctx context.Context, request *boltzrpc.CreateWalletRequest) (*boltzrpc.WalletCredentials, error) {
	mnemonic, err := wallet.GenerateMnemonic()
	if err != nil {
		return nil, handleError(errors.New("could not generate new mnemonic: " + err.Error()))
	}

	credentials := &boltzrpc.WalletCredentials{
		Mnemonic: &mnemonic,
	}

	if _, err := server.ImportWallet(ctx, &boltzrpc.ImportWalletRequest{
		Info: request.Info,
		Credentials: &boltzrpc.WalletCredentials{
			Mnemonic: &mnemonic,
		},
		Password: request.Password,
	}); err != nil {
		return nil, err
	}

	response, err := server.SetSubaccount(ctx, &boltzrpc.SetSubaccountRequest{
		Name: request.Info.Name,
	})
	if err != nil {
		return nil, err
	}
	credentials.Subaccount = &response.Pointer
	return credentials, nil
}

func (server *routedBoltzServer) serializeWallet(wal onchain.Wallet) (*boltzrpc.Wallet, error) {
	result := &boltzrpc.Wallet{
		Name:     wal.Name(),
		Currency: serializeCurrency(wal.Currency()),
		Readonly: wal.Readonly(),
	}
	balance, err := wal.GetBalance()
	if err != nil {
		if !errors.Is(err, wallet.ErrSubAccountNotSet) {
			return nil, handleError(fmt.Errorf("could not get balance for wallet %s: %w", wal.Name(), err))
		}
	} else {
		result.Balance = serializeWalletBalance(balance)
	}
	return result, nil
}

func (server *routedBoltzServer) GetWallet(_ context.Context, request *boltzrpc.GetWalletRequest) (*boltzrpc.Wallet, error) {
	wallet, err := server.onchain.GetWallet(request.Name, "", true)
	if err != nil {
		return nil, handleError(err)
	}

	return server.serializeWallet(wallet)
}

func (server *routedBoltzServer) GetWallets(_ context.Context, request *boltzrpc.GetWalletsRequest) (*boltzrpc.Wallets, error) {
	var response boltzrpc.Wallets
	currency := utils.ParseCurrency(request.Currency)
	for _, current := range server.onchain.Wallets {
		if (currency == "" || current.Currency() == currency) && (!current.Readonly() || request.GetIncludeReadonly()) {
			wallet, err := server.serializeWallet(current)
			if err != nil {
				return nil, err
			}
			response.Wallets = append(response.Wallets, wallet)
		}
	}
	return &response, nil
}

func (server *routedBoltzServer) GetWalletCredentials(_ context.Context, request *boltzrpc.GetWalletCredentialsRequest) (*boltzrpc.WalletCredentials, error) {
	creds, err := server.database.GetWalletCredentials(request.Name)
	if err != nil {
		return nil, handleError(fmt.Errorf("could not read credentials for wallet %s: %w", request.Name, err))
	}
	if creds.Encrypted() {
		creds, err = creds.Decrypt(request.GetPassword())
		if err != nil {
			return nil, handleError(fmt.Errorf("invalid password: %w", err))
		}
	}

	return serializeWalletCredentials(creds), err
}

func (server *routedBoltzServer) RemoveWallet(_ context.Context, request *boltzrpc.RemoveWalletRequest) (*boltzrpc.RemoveWalletResponse, error) {
	if err := server.database.DeleteWalletCredentials(request.Name); err != nil {
		return nil, handleError(err)
	}
	if server.swapper != nil {
		cfg, err := server.swapper.GetConfig()
		if err == nil {
			if cfg.Wallet == request.Name {
				return nil, handleError(fmt.Errorf(
					"wallet %s is used in autoswap, configure a different wallet in autoswap before removing this wallet.",
					request.Name,
				))
			}
		}
	}
	wallet, err := server.getOwnWallet(request.Name, true)
	if err != nil {
		return nil, handleError(err)
	}
	if err := wallet.Remove(); err != nil {
		return nil, handleError(err)
	}
	server.onchain.RemoveWallet(request.Name)

	logger.Debugf("Removed wallet %s", request.Name)

	return &boltzrpc.RemoveWalletResponse{}, nil
}

func (server *routedBoltzServer) Stop(context.Context, *empty.Empty) (*empty.Empty, error) {
	server.nursery.Stop()
	logger.Debugf("Stopped nursery")
	server.stop <- true
	return &empty.Empty{}, nil
}

func (server *routedBoltzServer) decryptWalletCredentials(password string) (decrypted []*wallet.Credentials, err error) {
	credentials, err := server.database.QueryWalletCredentials()
	if err != nil {
		return nil, err
	}
	for _, creds := range credentials {
		if creds.Encrypted() {
			if creds, err = creds.Decrypt(password); err != nil {
				logger.Debugf("failed to decrypted wallet credentials: %s", err)
				return nil, status.Errorf(codes.InvalidArgument, "wrong password")
			}
		}
		decrypted = append(decrypted, creds)
	}
	return decrypted, nil
}

func (server *routedBoltzServer) encryptWalletCredentials(password string, credentials []*wallet.Credentials) (err error) {
	tx, err := server.database.BeginTx()
	if err != nil {
		return err
	}
	for _, creds := range credentials {
		if password != "" {
			if creds, err = creds.Encrypt(password); err != nil {
				return err
			}
		}
		if err := tx.UpdateWalletCredentials(creds); err != nil {
			return tx.Rollback(err)
		}
	}
	return tx.Commit()
}

func (server *routedBoltzServer) Unlock(_ context.Context, request *boltzrpc.UnlockRequest) (*empty.Empty, error) {
	return &empty.Empty{}, handleError(server.unlock(request.Password))
}

func (server *routedBoltzServer) VerifyWalletPassword(_ context.Context, request *boltzrpc.VerifyWalletPasswordRequest) (*boltzrpc.VerifyWalletPasswordResponse, error) {
	_, err := server.decryptWalletCredentials(request.Password)
	return &boltzrpc.VerifyWalletPasswordResponse{Correct: err == nil}, nil
}

func (server *routedBoltzServer) unlock(password string) error {
	if !server.locked {
		return errors.New("boltzd already unlocked!")
	}

	credentials, err := server.decryptWalletCredentials(password)
	if err != nil {
		return err
	}
	for _, creds := range credentials {
		wallet, err := wallet.Login(creds)
		if err != nil {
			return fmt.Errorf("could not login to wallet: %v", err)
		} else {
			server.onchain.AddWallet(wallet)
		}
	}

	if err := server.swapper.LoadConfig(); err != nil {
		logger.Warnf("Could not load autoswap config: %v", err)
	}
	server.nursery = &nursery.Nursery{}
	err = server.nursery.Init(
		server.network,
		server.lightning,
		server.onchain,
		server.boltz,
		server.database,
	)
	if err != nil {
		return err
	}
	server.locked = false

	return nil
}

func (server *routedBoltzServer) ChangeWalletPassword(_ context.Context, request *boltzrpc.ChangeWalletPasswordRequest) (*empty.Empty, error) {
	decrypted, err := server.decryptWalletCredentials(request.Old)
	if err != nil {
		return nil, handleError(err)
	}

	if err := server.encryptWalletCredentials(request.New, decrypted); err != nil {
		return nil, handleError(err)
	}
	return &empty.Empty{}, nil
}

var errLocked = errors.New("boltzd is locked, use \"unlock\" to enable full RPC access")

func (server *routedBoltzServer) requestAllowed(fullMethod string) error {
	if server.locked && !strings.Contains(fullMethod, "Unlock") {
		return handleError(errLocked)
	}
	return nil
}

func (server *routedBoltzServer) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		if err := server.requestAllowed(info.FullMethod); err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

func (server *routedBoltzServer) StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		if err := server.requestAllowed(info.FullMethod); err != nil {
			return err
		}

		return handler(srv, ss)
	}
}

func (server *routedBoltzServer) getOwnWallet(name string, readonly bool) (*wallet.Wallet, error) {
	existing, err := server.onchain.GetWallet(name, "", readonly)
	if err != nil {
		return nil, err
	}
	wallet, ok := existing.(*wallet.Wallet)
	if !ok {
		return nil, fmt.Errorf("wallet %s can not be modified", name)
	}
	return wallet, nil
}

func findPair[T any](pair boltz.Pair, nested map[boltz.Currency]map[boltz.Currency]T) (*T, error) {
	from, hasPair := nested[pair.From]
	if !hasPair {
		return nil, fmt.Errorf("could not find pair from %v", pair)
	}
	result, hasPair := from[pair.To]
	if !hasPair {
		return nil, fmt.Errorf("could not find pair to %v", pair)
	}
	return &result, nil
}

func (server *routedBoltzServer) GetSubmarinePair(ctx context.Context, request *boltzrpc.Pair) (*boltzrpc.SubmarinePair, error) {
	pairsResponse, err := server.boltz.GetSubmarinePairs()
	if err != nil {
		return nil, handleError(err)
	}
	pair := utils.ParsePair(request)
	submarinePair, err := findPair(pair, pairsResponse)
	if err != nil {
		return nil, handleError(err)
	}

	return serializeSubmarinePair(pair, submarinePair), nil
}

func (server *routedBoltzServer) GetReversePair(ctx context.Context, request *boltzrpc.Pair) (*boltzrpc.ReversePair, error) {
	pairsResponse, err := server.boltz.GetReversePairs()
	if err != nil {
		return nil, err
	}
	pair := utils.ParsePair(request)
	reversePair, err := findPair(pair, pairsResponse)
	if err != nil {
		return nil, err
	}

	return serializeReversePair(pair, reversePair), nil
}

func (server *routedBoltzServer) GetPairs(context.Context, *empty.Empty) (*boltzrpc.GetPairsResponse, error) {
	response := &boltzrpc.GetPairsResponse{}

	submarinePairs, err := server.boltz.GetSubmarinePairs()
	if err != nil {
		return nil, err
	}

	for from, p := range submarinePairs {
		for to, pair := range p {
			if from != boltz.CurrencyRootstock {
				response.Submarine = append(response.Submarine, serializeSubmarinePair(boltz.Pair{
					From: from,
					To:   to,
				}, &pair))
			}
		}
	}

	reversePairs, err := server.boltz.GetReversePairs()
	if err != nil {
		return nil, err
	}

	for from, p := range reversePairs {
		for to, pair := range p {
			if to != boltz.CurrencyRootstock {
				response.Reverse = append(response.Reverse, serializeReversePair(boltz.Pair{
					From: from,
					To:   to,
				}, &pair))
			}
		}
	}

	return response, nil

}

func (server *routedBoltzServer) getPairs(pairId boltz.Pair) (*boltzrpc.Fees, *boltzrpc.Limits, error) {
	pairsResponse, err := server.boltz.GetPairs()

	if err != nil {
		return nil, nil, err
	}

	pair, hasPair := pairsResponse.Pairs[pairId.String()]

	if !hasPair {
		return nil, nil, fmt.Errorf("could not find pair with id %s", pairId)
	}

	minerFees := pair.Fees.MinerFees.BaseAsset

	return &boltzrpc.Fees{
			Percentage: pair.Fees.Percentage,
			Miner: &boltzrpc.MinerFees{
				Normal:  uint32(minerFees.Normal),
				Reverse: uint32(minerFees.Reverse.Lockup + minerFees.Reverse.Claim),
			},
		}, &boltzrpc.Limits{
			Minimal: pair.Limits.Minimal,
			Maximal: pair.Limits.Maximal,
		}, nil
}

func calculateDepositLimit(limit uint64, fees *boltzrpc.Fees, isMin bool) uint64 {
	effectiveRate := 1 + float64(fees.Percentage)/100
	limitFloat := float64(limit) * effectiveRate

	if isMin {
		// Add two more sats as safety buffer
		limitFloat = math.Ceil(limitFloat) + 2
	} else {
		limitFloat = math.Floor(limitFloat)
	}

	return uint64(limitFloat) + uint64(fees.Miner.Normal)
}

func newKeys() (*btcec.PrivateKey, *btcec.PublicKey, error) {
	privateKey, err := btcec.NewPrivateKey()

	if err != nil {
		return nil, nil, err
	}

	publicKey := privateKey.PubKey()

	return privateKey, publicKey, err
}

func newPreimage() ([]byte, []byte, error) {
	preimage := make([]byte, 32)
	_, err := rand.Read(preimage)

	if err != nil {
		return nil, nil, err
	}

	preimageHash := sha256.Sum256(preimage)

	return preimage, preimageHash[:], nil
}

func marshalJson(data interface{}) string {
	marshalled, _ := json.MarshalIndent(data, "", "  ")
	return string(marshalled)
}

func checkName(name string) error {
	if matched, err := regexp.MatchString("[^a-zA-Z\\d]", name); matched || err != nil {
		return errors.New("wallet name must only contain alphabetic characters and numbers")
	}
	return nil
}
