package electrum

import (
	"context"
	"crypto/tls"
	"time"

	"github.com/BoltzExchange/boltz-client/logger"
	"github.com/BoltzExchange/boltz-client/onchain"
	"github.com/checksum0/go-electrum/electrum"
)

type Client struct {
	client *electrum.Client
	ctx    context.Context

	blockHeight uint32
}

func NewClient(url string, ssl bool) (*Client, error) {
	// Establishing a new SSL connection to an ElectrumX server
	ctx := context.Background()
	c := &Client{ctx: ctx}
	var err error
	if ssl {
		c.client, err = electrum.NewClientSSL(ctx, url, &tls.Config{})
	} else {
		c.client, err = electrum.NewClientTCP(ctx, url)
	}
	if err != nil {
		return nil, err
	}

	// Making sure connection is not closed with timed "client.ping" call
	go func() {
		for {
			if err := c.client.Ping(ctx); err != nil {
				logger.Errorf("failed to ping electrum server: %s", err)
			}
			time.Sleep(60 * time.Second)
		}
	}()

	// Making sure we declare to the server what protocol we want to use
	if _, _, err := c.client.ServerVersion(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) RegisterBlockListener(channel chan<- *onchain.BlockEpoch, stop <-chan bool) error {
	results, err := c.client.SubscribeHeaders(c.ctx)
	if err != nil {
		return err
	}
	for {
		select {
		case <-stop:
			return nil
		case result := <-results:
			c.blockHeight = uint32(result.Height)
			channel <- &onchain.BlockEpoch{Height: c.blockHeight}
		}
	}
}
func (c *Client) GetBlockHeight() (uint32, error) {
	return c.blockHeight, nil
}

func (c *Client) EstimateFee(confTarget int32) (float64, error) {
	fee, err := c.client.GetFee(c.ctx, uint32(confTarget))
	return float64(fee), err
}
