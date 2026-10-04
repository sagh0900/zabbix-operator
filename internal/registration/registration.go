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

// Package registration keeps Zabbix proxies registered through the Zabbix API: it parses
// the proxy list, adds the in-cluster proxies and decides what to create, update and prune.
package registration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"sigs.k8s.io/yaml"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/system"
	"github.com/sagh0900/zabbix-operator/internal/zabbixapi"
)

// Modes of a listed proxy.
const (
	ModeActive  = "active"
	ModePassive = "passive"
)

// defaultPort is the port of a passive proxy and of a grouped proxy's local address.
const defaultPort = 10051

// Entry is one proxy to keep registered.
type Entry struct {
	// Name is the proxy's Hostname.
	Name string `json:"name"`
	// Mode is active (default) or passive.
	Mode string `json:"mode,omitempty"`
	// Address and Port are where the server reaches a passive proxy.
	Address string `json:"address,omitempty"`
	Port    int    `json:"port,omitempty"`
	// ProxyGroup is a proxy group to join; it is created if missing.
	ProxyGroup string `json:"proxyGroup,omitempty"`
	// LocalAddress and LocalPort are where agents reach a grouped proxy. LocalAddress
	// defaults to Address.
	LocalAddress string `json:"localAddress,omitempty"`
	LocalPort    int    `json:"localPort,omitempty"`
	// Description is shown in the Zabbix frontend.
	Description string `json:"description,omitempty"`
}

// Parse reads a YAML list of proxies, applies defaults and validates it.
func Parse(data []byte) ([]Entry, error) {
	var list []Entry
	if err := yaml.UnmarshalStrict(data, &list); err != nil {
		return nil, fmt.Errorf("parsing the proxy list: %w", err)
	}
	for i := range list {
		if err := list[i].normalize(); err != nil {
			return nil, fmt.Errorf("proxy %d (%q): %w", i+1, list[i].Name, err)
		}
	}
	return list, nil
}

func (e *Entry) normalize() error {
	if e.Name == "" || len(e.Name) > 128 {
		return errors.New("name must be 1 to 128 characters")
	}
	switch e.Mode {
	case "":
		e.Mode = ModeActive
	case ModeActive, ModePassive:
	default:
		return fmt.Errorf("mode must be %s or %s", ModeActive, ModePassive)
	}
	if e.Mode == ModePassive && e.Address == "" {
		return errors.New("address is required for a passive proxy")
	}
	if e.Port == 0 {
		e.Port = defaultPort
	}
	if e.ProxyGroup != "" {
		if e.LocalAddress == "" {
			e.LocalAddress = e.Address
		}
		if e.LocalAddress == "" {
			return errors.New("localAddress (or address) is required for a proxy in a proxy group")
		}
		if e.LocalPort == 0 {
			e.LocalPort = defaultPort
		}
	}
	for _, p := range []int{e.Port, e.LocalPort} {
		if p < 0 || p > 65535 {
			return fmt.Errorf("port %d out of range", p)
		}
	}
	return nil
}

// Desired returns the listed proxies plus every instance of the system's enabled
// in-cluster proxies. A name may appear only once.
func Desired(sys *zabbixv1alpha1.ZabbixSystem, listed []Entry) ([]Entry, error) {
	out := slices.Clone(listed)
	for i := range sys.Spec.Proxies {
		p := &sys.Spec.Proxies[i]
		if !p.IsEnabled() {
			continue
		}
		replicas := max(int(p.Replicas), 1)
		for j := 0; j < replicas; j++ {
			e := Entry{Name: system.ProxyPodName(p, j), Mode: ModeActive, Port: defaultPort, ProxyGroup: p.ProxyGroup,
				Description: fmt.Sprintf("In-cluster proxy of ZabbixSystem %s/%s", sys.Namespace, sys.Name)}
			addr := system.ProxyAddress(sys, p, j)
			if p.Mode == zabbixv1alpha1.ProxyPassive {
				e.Mode, e.Address = ModePassive, addr
			}
			if e.ProxyGroup != "" {
				e.LocalAddress, e.LocalPort = addr, defaultPort
			}
			out = append(out, e)
		}
	}
	seen := map[string]bool{}
	for _, e := range out {
		if seen[e.Name] {
			return nil, fmt.Errorf("proxy %q is listed more than once", e.Name)
		}
		seen[e.Name] = true
	}
	return out, nil
}

// API is the part of the Zabbix API that registration uses.
type API interface {
	Proxies(ctx context.Context) ([]zabbixapi.Proxy, error)
	CreateProxy(ctx context.Context, p zabbixapi.Proxy) error
	UpdateProxy(ctx context.Context, p zabbixapi.Proxy) error
	DeleteProxies(ctx context.Context, ids []string) error
	ProxyGroups(ctx context.Context) ([]zabbixapi.ProxyGroup, error)
	CreateProxyGroup(ctx context.Context, name, description string) (string, error)
}

// Result is the outcome of a Sync.
type Result struct {
	// Registered are the proxies this system now manages, sorted.
	Registered []string
	// Created, Updated and Deleted count the changes made.
	Created, Updated, Deleted int
}

// Plan is what a Sync changes, decided from the desired and existing proxies.
type Plan struct {
	Create []zabbixapi.Proxy
	Update []zabbixapi.Proxy
	// Delete are the IDs and names of proxies to prune.
	Delete     []string
	DeleteName []string
	// Keep are previously registered proxies that are no longer listed but not pruned.
	Keep []string
}

// Decide plans a Sync. Listed proxies that exist are adopted and updated when they
// differ; registered proxies no longer listed are pruned when prune is set and kept on
// record otherwise. Proxies neither listed nor registered are never touched. groups maps
// proxy group names to IDs and must contain every group the desired proxies use.
func Decide(desired []Entry, existing []zabbixapi.Proxy, groups map[string]string, registered []string, prune bool) Plan {
	byName := map[string]zabbixapi.Proxy{}
	for _, p := range existing {
		byName[p.Name] = p
	}
	var plan Plan
	listed := map[string]bool{}
	for _, e := range desired {
		listed[e.Name] = true
		want := e.proxy(groups)
		cur, ok := byName[e.Name]
		switch {
		case !ok:
			plan.Create = append(plan.Create, want)
		case !same(cur, want):
			want.ProxyID = cur.ProxyID
			plan.Update = append(plan.Update, want)
		}
	}
	for _, name := range registered {
		cur, ok := byName[name]
		if listed[name] || !ok {
			continue
		}
		if prune {
			plan.Delete = append(plan.Delete, cur.ProxyID)
			plan.DeleteName = append(plan.DeleteName, name)
		} else {
			plan.Keep = append(plan.Keep, name)
		}
	}
	return plan
}

// proxy renders the entry as the API object.
func (e Entry) proxy(groups map[string]string) zabbixapi.Proxy {
	p := zabbixapi.Proxy{Name: e.Name, OperatingMode: zabbixapi.ModeActive, ProxyGroupID: "0", Description: e.Description}
	if e.Mode == ModePassive {
		p.OperatingMode, p.Address, p.Port = zabbixapi.ModePassive, e.Address, strconv.Itoa(e.Port)
	}
	if e.ProxyGroup != "" {
		p.ProxyGroupID = groups[e.ProxyGroup]
		p.LocalAddress, p.LocalPort = e.LocalAddress, strconv.Itoa(e.LocalPort)
	}
	return p
}

// same compares the fields the operator manages; address and port only matter for
// passive proxies, local address and port only inside a group.
func same(cur, want zabbixapi.Proxy) bool {
	group := func(id string) string {
		if id == "" {
			return "0"
		}
		return id
	}
	if cur.OperatingMode != want.OperatingMode || group(cur.ProxyGroupID) != group(want.ProxyGroupID) || cur.Description != want.Description {
		return false
	}
	if want.OperatingMode == zabbixapi.ModePassive && (cur.Address != want.Address || cur.Port != want.Port) {
		return false
	}
	if group(want.ProxyGroupID) != "0" && (cur.LocalAddress != want.LocalAddress || cur.LocalPort != want.LocalPort) {
		return false
	}
	return true
}

// Sync makes Zabbix match desired. It applies every change it can and returns the first
// error; the result always reflects what was registered, so a partial failure is
// recorded and retried.
func Sync(ctx context.Context, api API, desired []Entry, registered []string, prune bool, groupDescription string) (Result, error) {
	var res Result
	groups, err := ensureGroups(ctx, api, desired, groupDescription)
	if err != nil {
		return Result{Registered: registered}, err
	}
	existing, err := api.Proxies(ctx)
	if err != nil {
		return Result{Registered: registered}, err
	}
	plan := Decide(desired, existing, groups, registered, prune)

	have := map[string]bool{}
	for _, p := range existing {
		have[p.Name] = true
	}
	var errs []error
	failed := map[string]bool{}
	for _, p := range plan.Create {
		if err := api.CreateProxy(ctx, p); err != nil {
			errs = append(errs, fmt.Errorf("creating proxy %q: %w", p.Name, err))
			failed[p.Name] = true
			continue
		}
		res.Created++
	}
	for _, p := range plan.Update {
		if err := api.UpdateProxy(ctx, p); err != nil {
			errs = append(errs, fmt.Errorf("updating proxy %q: %w", p.Name, err))
			continue
		}
		res.Updated++
	}
	reg := slices.Clone(plan.Keep)
	if len(plan.Delete) > 0 {
		if err := api.DeleteProxies(ctx, plan.Delete); err != nil {
			errs = append(errs, fmt.Errorf("pruning proxies %v: %w", plan.DeleteName, err))
			reg = append(reg, plan.DeleteName...)
		} else {
			res.Deleted = len(plan.Delete)
		}
	}
	for _, e := range desired {
		if have[e.Name] || !failed[e.Name] {
			reg = append(reg, e.Name)
		}
	}
	slices.Sort(reg)
	res.Registered = slices.Compact(reg)
	return res, errors.Join(errs...)
}

// ensureGroups returns the IDs of the proxy groups the desired proxies use, creating
// missing ones.
func ensureGroups(ctx context.Context, api API, desired []Entry, description string) (map[string]string, error) {
	var names []string
	for _, e := range desired {
		if e.ProxyGroup != "" && !slices.Contains(names, e.ProxyGroup) {
			names = append(names, e.ProxyGroup)
		}
	}
	ids := map[string]string{}
	if len(names) == 0 {
		return ids, nil
	}
	list, err := api.ProxyGroups(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range list {
		ids[g.Name] = g.ProxyGroupID
	}
	for _, n := range names {
		if _, ok := ids[n]; ok {
			continue
		}
		id, err := api.CreateProxyGroup(ctx, n, description)
		if err != nil {
			return nil, fmt.Errorf("creating proxy group %q: %w", n, err)
		}
		ids[n] = id
	}
	return ids, nil
}
