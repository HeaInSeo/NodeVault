// Package registry handles internal container registry interactions.
// Responsible for verifying push success and acquiring image digests
// via the OCI Distribution Spec registry API.
package registry

import (
	"fmt"
	"net/http"
	"strings"
)

// acceptHeaders lists the manifest media types to request, in preference order.
var acceptHeaders = []string{
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.oci.image.index.v1+json",
}

// Client queries the internal container registry.
type Client struct {
	http *http.Client
}

// NewClient creates a registry Client using the default HTTP client.
func NewClient() *Client {
	return &Client{http: http.DefaultClient}
}

// newClientWithHTTP creates a registry Client with a custom HTTP client (for testing).
func newClientWithHTTP(h *http.Client) *Client {
	return &Client{http: h}
}

// parseDestination splits "host/name:tag" into its three components.
func parseDestination(destination string) (host, name, tag string, err error) {
	slash := strings.SplitN(destination, "/", 2)
	if len(slash) != 2 {
		return "", "", "", fmt.Errorf("invalid destination (no '/'): %s", destination)
	}
	host = slash[0]
	nameTag := strings.SplitN(slash[1], ":", 2)
	if len(nameTag) != 2 {
		return "", "", "", fmt.Errorf("invalid destination (no ':'): %s", destination)
	}
	return host, nameTag[0], nameTag[1], nil
}
