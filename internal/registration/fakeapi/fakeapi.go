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

// Package fakeapi is an in-memory Zabbix API for registration tests.
package fakeapi

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"

	"github.com/sagh0900/zabbix-operator/internal/zabbixapi"
)

// API stores proxies and proxy groups in memory and counts the calls that change them.
type API struct {
	mu      sync.Mutex
	next    int
	proxies map[string]zabbixapi.Proxy // by ID
	groups  map[string]string          // name -> ID
	// Fail makes calls of the named method ("proxy.create", ...) return an error.
	Fail map[string]bool
	// Calls counts calls by method.
	Calls map[string]int
}

// New returns an empty API.
func New() *API {
	return &API{proxies: map[string]zabbixapi.Proxy{}, groups: map[string]string{}, Fail: map[string]bool{}, Calls: map[string]int{}}
}

func (a *API) call(method string) error {
	a.Calls[method]++
	if a.Fail[method] {
		return &zabbixapi.Error{Code: -32500, Message: "Application error.", Data: method + " failed"}
	}
	return nil
}

func (a *API) id() string { a.next++; return strconv.Itoa(a.next) }

// Add stores a proxy as if it had been created by hand and returns its ID.
func (a *API) Add(p zabbixapi.Proxy) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	p.ProxyID = a.id()
	if p.ProxyGroupID == "" {
		p.ProxyGroupID = "0"
	}
	a.proxies[p.ProxyID] = p
	return p.ProxyID
}

// Get returns the proxy with the given name.
func (a *API) Get(name string) (zabbixapi.Proxy, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.proxies {
		if p.Name == name {
			return p, true
		}
	}
	return zabbixapi.Proxy{}, false
}

// Names returns the sorted proxy names.
func (a *API) Names() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.proxies))
	for _, p := range a.proxies {
		out = append(out, p.Name)
	}
	slices.Sort(out)
	return out
}

// Group returns the ID of a proxy group.
func (a *API) Group(name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.groups[name]
}

// SetFail makes a method fail or succeed.
func (a *API) SetFail(method string, fail bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Fail[method] = fail
}

// Count returns the number of calls of a method.
func (a *API) Count(method string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Calls[method]
}

func (a *API) Proxies(context.Context) ([]zabbixapi.Proxy, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.call("proxy.get"); err != nil {
		return nil, err
	}
	out := make([]zabbixapi.Proxy, 0, len(a.proxies))
	for _, p := range a.proxies {
		out = append(out, p)
	}
	return out, nil
}

func (a *API) CreateProxy(_ context.Context, p zabbixapi.Proxy) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.call("proxy.create"); err != nil {
		return err
	}
	for _, q := range a.proxies {
		if q.Name == p.Name {
			return errors.New("proxy already exists")
		}
	}
	p.ProxyID = a.id()
	a.proxies[p.ProxyID] = p
	return nil
}

func (a *API) UpdateProxy(_ context.Context, p zabbixapi.Proxy) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.call("proxy.update"); err != nil {
		return err
	}
	if _, ok := a.proxies[p.ProxyID]; !ok {
		return errors.New("no such proxy")
	}
	a.proxies[p.ProxyID] = p
	return nil
}

func (a *API) DeleteProxies(_ context.Context, ids []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.call("proxy.delete"); err != nil {
		return err
	}
	for _, id := range ids {
		delete(a.proxies, id)
	}
	return nil
}

func (a *API) ProxyGroups(context.Context) ([]zabbixapi.ProxyGroup, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.call("proxygroup.get"); err != nil {
		return nil, err
	}
	var out []zabbixapi.ProxyGroup
	for n, id := range a.groups {
		out = append(out, zabbixapi.ProxyGroup{ProxyGroupID: id, Name: n})
	}
	return out, nil
}

func (a *API) CreateProxyGroup(_ context.Context, name, _ string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.call("proxygroup.create"); err != nil {
		return "", err
	}
	id := a.id()
	a.groups[name] = id
	return id, nil
}
