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

package zabbixapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// server answers JSON-RPC calls with handle and records the requests.
func server(t *testing.T, handle func(method string, params json.RawMessage) (any, *Error)) (*Client, *[]string) {
	t.Helper()
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json-rpc" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
			ID      int64           `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.JSONRPC != "2.0" || req.ID == 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		auth = append(auth, req.Method+" "+r.Header.Get("Authorization"))
		result, rpcErr := handle(req.Method, req.Params)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL+"/api_jsonrpc.php", "secret-token"), &auth
}

func TestVersionNeedsNoToken(t *testing.T) {
	c, auth := server(t, func(string, json.RawMessage) (any, *Error) { return "8.0.0", nil })
	v, err := c.Version(context.Background())
	if err != nil || v != "8.0.0" {
		t.Fatalf("Version = %q, %v", v, err)
	}
	if (*auth)[0] != "apiinfo.version " {
		t.Errorf("apiinfo.version must be sent without a token, got %q", (*auth)[0])
	}
}

func TestProxyCalls(t *testing.T) {
	var got = map[string]map[string]any{}
	c, auth := server(t, func(method string, params json.RawMessage) (any, *Error) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		got[method] = p
		switch method {
		case "proxy.get":
			return []map[string]string{{"proxyid": "7", "name": "dc1-0", "operating_mode": "0", "proxy_groupid": "0"}}, nil
		case "proxygroup.create":
			return map[string][]string{"proxy_groupids": {"3"}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	ctx := context.Background()
	list, err := c.Proxies(ctx)
	if err != nil || len(list) != 1 || list[0].ProxyID != "7" || list[0].OperatingMode != ModeActive {
		t.Fatalf("Proxies = %+v, %v", list, err)
	}
	if err := c.CreateProxy(ctx, Proxy{Name: "edge-0", OperatingMode: ModePassive, Address: "edge", Port: "10051"}); err != nil {
		t.Fatal(err)
	}
	if p := got["proxy.create"]; p["address"] != "edge" || p["port"] != "10051" || p["proxy_groupid"] != "0" || p["proxyid"] != nil {
		t.Errorf("proxy.create params = %v", p)
	}
	if err := c.UpdateProxy(ctx, Proxy{ProxyID: "7", Name: "dc1-0", OperatingMode: ModeActive, Address: "ignored",
		ProxyGroupID: "3", LocalAddress: "dc1-0.dc1", LocalPort: "10051"}); err != nil {
		t.Fatal(err)
	}
	if p := got["proxy.update"]; p["proxyid"] != "7" || p["address"] != nil || p["local_address"] != "dc1-0.dc1" || p["proxy_groupid"] != "3" {
		t.Errorf("proxy.update params = %v", p)
	}
	if id, err := c.CreateProxyGroup(ctx, "dc", ""); err != nil || id != "3" {
		t.Errorf("CreateProxyGroup = %q, %v", id, err)
	}
	if err := c.DeleteProxies(ctx, nil); err != nil {
		t.Fatal(err)
	}
	for _, a := range *auth {
		if a[len(a)-len("Bearer secret-token"):] != "Bearer secret-token" {
			t.Errorf("call without the token: %q", a)
		}
	}
	if len(*auth) != 4 {
		t.Errorf("DeleteProxies with no IDs must not call the API; calls: %v", *auth)
	}
}

func TestErrors(t *testing.T) {
	c, _ := server(t, func(string, json.RawMessage) (any, *Error) {
		return nil, &Error{Code: -32602, Message: "Invalid params.", Data: "Proxy \"x\" already exists."}
	})
	err := c.CreateProxy(context.Background(), Proxy{Name: "x", OperatingMode: ModeActive})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != -32602 {
		t.Fatalf("error = %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "t").Proxies(context.Background()); err == nil {
		t.Fatal("HTTP 403 must be an error")
	}
}
