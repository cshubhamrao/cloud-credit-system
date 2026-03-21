package ledger

import (
	"fmt"

	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
	"github.com/tigerbeetle/tigerbeetle-go/pkg/types"
)

// Client wraps the TigerBeetle client with convenience helpers.
type Client struct {
	tb tigerbeetle.Client
}

// NewClient connects to a TigerBeetle cluster.
// clusterID must match the cluster ID used when formatting the data file.
func NewClient(clusterID uint32, address string) (*Client, error) {
	tb, err := tigerbeetle.NewClient(types.ToUint128(uint64(clusterID)), []string{address})
	if err != nil {
		return nil, fmt.Errorf("tigerbeetle.NewClient: %w", err)
	}
	return &Client{tb: tb}, nil
}

// Close releases the TigerBeetle client resources.
func (c *Client) Close() {
	c.tb.Close()
}

// Raw returns the underlying tigerbeetle.Client for direct use in ledger helpers.
func (c *Client) Raw() tigerbeetle.Client {
	return c.tb
}
