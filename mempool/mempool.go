package mempool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/BoltzExchange/boltz-client/logger"
	"github.com/BoltzExchange/boltz-client/onchain"
	"github.com/btcsuite/websocket"
)

type feeEstimation struct {
	FastestFee  float64 `json:"fastestFee"`
	HalfHourFee float64 `json:"halfHourFee"`
	HourFee     float64 `json:"hourFee"`
	EconomyFee  float64 `json:"economyFee"`
	MinimumFee  float64 `json:"minimumFee"`
}

type blockResponse struct {
	Block struct {
		Height uint32 `json:"height"`
	} `json:"block"`
}

type Client struct {
	api   string
	apiv1 string
}

func InitClient(endpoint string) *Client {
	endpointStripped := strings.TrimSuffix(endpoint, "/")
	endpointV1 := endpointStripped
	if !strings.HasSuffix(endpointV1, "/v1") {
		endpointV1 += "/v1"
	}

	return &Client{
		api:   endpointStripped,
		apiv1: endpointV1,
	}
}

func (c *Client) getFeeRecommendation() (*feeEstimation, error) {
	req, err := http.NewRequest(http.MethodGet, c.apiv1+"/fees/recommended", nil)
	if err != nil {
		return nil, err
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed with status: %d", res.StatusCode)
	}

	var fees feeEstimation
	err = json.NewDecoder(res.Body).Decode(&fees)
	if err != nil {
		return nil, err
	}

	return &fees, nil
}

func (c *Client) EstimateFee(confTarget int32) (float64, error) {
	fees, err := c.getFeeRecommendation()
	if err != nil {
		return 0, err
	}
	// TODO: take confTarget into consideration or refactor interface to take constants for "fast" or "slow" fee
	return fees.HalfHourFee, nil
}

func (c *Client) GetTxHex(txId string) (string, error) {
	res, err := http.Get(c.api + "/tx/" + txId + "/hex")
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("could not get tx %s, failed with status: %d", txId, res.StatusCode)
	}

	hex, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	return string(hex), nil
}

func (c *Client) RegisterBlockListener(channel chan<- *onchain.BlockEpoch, stop <-chan bool) error {
	ws, err := url.Parse(c.apiv1)
	if err != nil {
		return err
	}
	ws.Path += "/ws"

	if ws.Scheme == "https" {
		ws.Scheme = "wss"
	} else if ws.Scheme == "http" {
		ws.Scheme = "ws"
	}

	logger.Info("Connecting to mempool websocket api: " + ws.String())

	conn, _, err := websocket.DefaultDialer.Dial(ws.String(), nil)
	if err != nil {
		return err
	}

	err = conn.WriteMessage(websocket.TextMessage, []byte(`{"action":"init"}`))
	if err != nil {
		return err
	}

	err = conn.WriteMessage(websocket.TextMessage, []byte(`{"action":"want", "data": ["blocks"]}`))
	if err != nil {
		return err
	}

	closed := false

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				err = conn.WriteMessage(websocket.PingMessage, nil)
				if err != nil {
					logger.Error("Could not ping mempool websocket: " + err.Error())
					return
				}
			case <-stop:
				closed = true
				if err := conn.Close(); err != nil {
					logger.Error("Could not close mempool websocket: " + err.Error())
				}
				return
			}
		}
	}()

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if closed {
				return nil
			}
			return errors.New("could not receive message: " + err.Error())
		}

		logger.Silly("Received websocket message: " + string(message))

		parsed := blockResponse{}

		err = json.Unmarshal(message, &parsed)
		if err != nil {
			return errors.New("could not parse block response: " + err.Error())
		}

		if parsed.Block.Height != 0 {
			channel <- &onchain.BlockEpoch{
				Height: parsed.Block.Height,
			}
		}
	}
}

func (c *Client) GetBlockHeight() (uint32, error) {
	res, err := http.Get(c.apiv1 + "/blocks/tip/height")
	if err != nil {
		return 0, err
	}
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("could not get block height, failed with status: %d", res.StatusCode)
	}

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, err
	}
	height, err := strconv.ParseUint(string(raw), 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(height), nil
}
