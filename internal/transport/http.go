package transport

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"git.sr.ht/~spc/go-log"
	"github.com/redhatinsights/yggdrasil"
	internalhttp "github.com/redhatinsights/yggdrasil/internal/http"
)

// HTTPResponse is a data structure representing an HTTP response received from
// an HTTP request sent through the transport.
type HTTPResponse struct {
	StatusCode int
	Body       json.RawMessage
	Metadata   map[string]string
}

// HTTP is a Transporter that sends and receives data and control
// messages by sending HTTP requests to a URL.
type HTTP struct {
	clientID        string
	client          *internalhttp.Client
	server          string
	dataHandler     DataReceiveHandlerFunc
	pollingInterval time.Duration
	disconnected    atomic.Value
	userAgent       string
	isTLS           atomic.Value
}

func NewHTTPTransport(clientID string, server string, tlsConfig *tls.Config, userAgent string, pollingInterval time.Duration, dataRecvFunc DataReceiveHandlerFunc) (*HTTP, error) {
	disconnected := atomic.Value{}
	disconnected.Store(false)
	isTls := atomic.Value{}
	isTls.Store(tlsConfig != nil)
	return &HTTP{
		clientID:        clientID,
		client:          internalhttp.NewHTTPClient(tlsConfig.Clone(), userAgent),
		dataHandler:     dataRecvFunc,
		pollingInterval: pollingInterval,
		disconnected:    disconnected,
		server:          server,
		userAgent:       userAgent,
		isTLS:           isTls,
	}, nil
}

func (t *HTTP) Connect() error {
	t.disconnected.Store(false)
	go func() {
		for {
			if t.disconnected.Load().(bool) {
				return
			}
			resp, err := t.client.Get(t.getUrl("in", "control"))
			if err != nil {
				log.Tracef("cannot get HTTP request: %v", err)
			}
			if resp != nil {
				data, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					log.Errorf("cannot read response body: %v", err)
					continue
				}
				_ = t.ReceiveData(data, "control")
				resp.Body.Close()
			}
			time.Sleep(t.pollingInterval)
		}
	}()

	go func() {
		for {
			if t.disconnected.Load().(bool) {
				return
			}
			resp, err := t.client.Get(t.getUrl("in", "data"))
			if err != nil {
				log.Tracef("cannot get HTTP request: %v", err)
			}

			if resp != nil {
				data, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					log.Errorf("cannot read response body: %v", err)
					continue
				}
				_ = t.ReceiveData(data, "data")
				resp.Body.Close()
			}
			time.Sleep(t.pollingInterval)
		}
	}()

	return nil
}

// ReloadTLSConfig creates a new HTTP client with the provided TLS config.
func (t *HTTP) ReloadTLSConfig(tlsConfig *tls.Config) error {
	*t.client = *internalhttp.NewHTTPClient(tlsConfig, t.userAgent)
	t.isTLS.Store(tlsConfig != nil)
	return nil
}

func (t *HTTP) Disconnect(quiesce uint) {
	time.Sleep(time.Millisecond * time.Duration(quiesce))
	t.disconnected.Store(true)
}

func (t *HTTP) SendData(data []byte, dest string) ([]byte, error) {
	return t.send(data, dest)
}

func (t *HTTP) ReceiveData(data []byte, dest string) error {
	t.dataHandler(data, dest)
	return nil
}

func (t *HTTP) send(message []byte, channel string) ([]byte, error) {
	if t.disconnected.Load().(bool) {
		return nil, nil
	}
	url := t.getUrl("out", channel)
	headers := map[string]string{
		"Content-Type": "application/json",
	}
	log.Tracef("posting HTTP request body: %s", string(message))
	res, err := t.client.Post(url, headers, message)
	if err != nil && res == nil {
		return nil, fmt.Errorf("cannot do HTTP request: %w", err)
	}

	var response HTTPResponse
	response.StatusCode = res.StatusCode
	response.Metadata = make(map[string]string)
	for k, v := range res.Header {
		response.Metadata[k] = strings.Join(v, ";")
	}
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("cannot read HTTP response body: %w", err)
	}
	defer res.Body.Close()

	if err := json.Unmarshal(body, &response.Body); err != nil {
		return nil, fmt.Errorf("cannot marshal HTTP response body: %w", err)
	}

	data, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("cannot marshal HTTP response: %w", err)
	}

	var httpError error
	if res.StatusCode >= 400 {
		httpError = fmt.Errorf("%v", http.StatusText(res.StatusCode))
	}

	return data, httpError
}

func (t *HTTP) getUrl(direction string, channel string) string {
	protocol := "http"
	if t.isTLS.Load().(bool) {
		protocol = "https"
	}
	path := filepath.Join(yggdrasil.PathPrefix, channel, t.clientID, direction)

	return fmt.Sprintf("%s://%s/%s", protocol, t.server, path)
}
