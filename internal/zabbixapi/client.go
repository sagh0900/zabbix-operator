/*
Copyright The Zabbix Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package zabbixapi is a minimal client for the Zabbix JSON-RPC API: the proxy and proxy
// group methods the operator needs, authenticated with an API token.
package zabbixapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// Client calls the Zabbix API at URL (…/api_jsonrpc.php) with an API token.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
	id    atomic.Int64
}

// New returns a client with a 15-second request timeout.
func New(url, token string) *Client {
	return &Client{URL: url, Token: token, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

// Error is an error returned by the Zabbix API.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("zabbix API error %d: %s %s", e.Code, e.Message, e.Data)
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
	ID      int64  `json:"id"`
}

type response struct {
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
}

// call invokes method with params and decodes the result into out.
func (c *Client) call(ctx context.Context, method string, params, out any, auth bool) error {
	body, err := json.Marshal(request{JSONRPC: "2.0", Method: method, Params: params, ID: c.id.Add(1)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json-rpc")
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("zabbix API %s: HTTP %d", method, resp.StatusCode)
	}
	var r response
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("zabbix API %s: %w", method, err)
	}
	if r.Error != nil {
		return r.Error
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.Result, out)
}

// Version returns the API version, which equals the Zabbix version. It needs no token.
func (c *Client) Version(ctx context.Context) (string, error) {
	var v string
	err := c.call(ctx, "apiinfo.version", map[string]any{}, &v, false)
	return v, err
}

// Proxy is a Zabbix proxy as the operator manages it. Numeric fields are strings, as the
// API returns them.
type Proxy struct {
	ProxyID       string `json:"proxyid,omitempty"`
	Name          string `json:"name"`
	OperatingMode string `json:"operating_mode"`
	Address       string `json:"address,omitempty"`
	Port          string `json:"port,omitempty"`
	ProxyGroupID  string `json:"proxy_groupid,omitempty"`
	LocalAddress  string `json:"local_address,omitempty"`
	LocalPort     string `json:"local_port,omitempty"`
	Description   string `json:"description"`
}

// keyDescription is the description field of proxies and proxy groups.
const keyDescription = "description"

// Operating modes of a proxy.
const (
	ModeActive  = "0"
	ModePassive = "1"
)

// Proxies returns every proxy.
func (c *Client) Proxies(ctx context.Context) ([]Proxy, error) {
	var out []Proxy
	err := c.call(ctx, "proxy.get", map[string]any{"output": "extend"}, &out, true)
	return out, err
}

// CreateProxy creates p.
func (c *Client) CreateProxy(ctx context.Context, p Proxy) error {
	p.ProxyID = ""
	return c.call(ctx, "proxy.create", proxyParams(p), nil, true)
}

// UpdateProxy updates the proxy p.ProxyID to p.
func (c *Client) UpdateProxy(ctx context.Context, p Proxy) error {
	params := proxyParams(p)
	params["proxyid"] = p.ProxyID
	return c.call(ctx, "proxy.update", params, nil, true)
}

// DeleteProxies deletes proxies by ID.
func (c *Client) DeleteProxies(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return c.call(ctx, "proxy.delete", ids, nil, true)
}

// proxyParams sends only the fields that apply to the proxy's mode and group.
func proxyParams(p Proxy) map[string]any {
	params := map[string]any{"name": p.Name, "operating_mode": p.OperatingMode, keyDescription: p.Description}
	if p.OperatingMode == ModePassive {
		params["address"], params["port"] = p.Address, p.Port
	}
	params["proxy_groupid"] = "0"
	if p.ProxyGroupID != "" && p.ProxyGroupID != "0" {
		params["proxy_groupid"] = p.ProxyGroupID
		params["local_address"], params["local_port"] = p.LocalAddress, p.LocalPort
	}
	return params
}

// ProxyGroup is a Zabbix proxy group.
type ProxyGroup struct {
	ProxyGroupID string `json:"proxy_groupid,omitempty"`
	Name         string `json:"name"`
}

// ProxyGroups returns every proxy group.
func (c *Client) ProxyGroups(ctx context.Context) ([]ProxyGroup, error) {
	var out []ProxyGroup
	err := c.call(ctx, "proxygroup.get", map[string]any{"output": "extend"}, &out, true)
	return out, err
}

// CreateProxyGroup creates a proxy group with Zabbix's defaults (failover delay 1m, one
// proxy online) and returns its ID.
func (c *Client) CreateProxyGroup(ctx context.Context, name, description string) (string, error) {
	var out struct {
		IDs []string `json:"proxy_groupids"`
	}
	if err := c.call(ctx, "proxygroup.create", map[string]any{
		"name": name, "failover_delay": "1m", "min_online": "1", keyDescription: description,
	}, &out, true); err != nil {
		return "", err
	}
	if len(out.IDs) != 1 {
		return "", fmt.Errorf("proxygroup.create returned %d IDs", len(out.IDs))
	}
	return out.IDs[0], nil
}
