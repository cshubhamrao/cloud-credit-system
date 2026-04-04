package ledger

import (
	"fmt"
	"net"

	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
	"github.com/tigerbeetle/tigerbeetle-go/pkg/types"
)

// Client wraps the TigerBeetle client with convenience helpers.
type Client struct {
	tb tigerbeetle.Client
}

// NewClient connects to a TigerBeetle cluster.
// clusterID must match the cluster ID used when formatting the data file.
// address may be a hostname:port — it is resolved to ip:port before being
// passed to the TigerBeetle client, which requires an IP address.
func NewClient(clusterID uint32, address string) (*Client, error) {
	resolved, err := resolveAddr(address)
	if err != nil {
		return nil, fmt.Errorf("resolve TB address %q: %w", address, err)
	}
	tb, err := tigerbeetle.NewClient(types.ToUint128(uint64(clusterID)), []string{resolved})
	if err != nil {
		return nil, fmt.Errorf("tigerbeetle.NewClient: %w", err)
	}
	return &Client{tb: tb}, nil
}

// resolveAddr resolves hostname:port to ip:port.
// IPv6 addresses are bracket-wrapped as required by host:port format.
// If the host is already an IP, it is returned unchanged.
func resolveAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, nil // not host:port, pass through
	}
	if net.ParseIP(host) != nil {
		return addr, nil // already an IP
	}
	ips, err := net.LookupHost(host)
	if err != nil || len(ips) == 0 {
		return "", fmt.Errorf("lookup %q: %w", host, err)
	}
	ip := net.ParseIP(ips[0])
	if ip == nil {
		return "", fmt.Errorf("invalid resolved IP %q", ips[0])
	}
	if ip.To4() == nil { // IPv6 needs brackets
		return "[" + ip.String() + "]:" + port, nil
	}
	return ip.String() + ":" + port, nil
}

// Close releases the TigerBeetle client resources.
func (c *Client) Close() {
	c.tb.Close()
}

// Raw returns the underlying tigerbeetle.Client for direct use in ledger helpers.
func (c *Client) Raw() tigerbeetle.Client {
	return c.tb
}
