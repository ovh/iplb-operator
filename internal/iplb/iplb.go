/*
Copyright 2026.

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

// Package iplb provides typed wrappers over the OVHcloud IP Load Balancing API
// (the /ipLoadbalancing/{serviceName}/... endpoints).
package iplb

import (
	"fmt"
	"net/url"

	"github.com/ovhcloud/iplb-operator/internal/ovhclient"
)

// Service is a typed client scoped to one IPLB service (serviceName).
type Service struct {
	api     ovhclient.Client
	service string
}

// New returns a Service bound to the given serviceName.
func New(api ovhclient.Client, serviceName string) *Service {
	return &Service{api: api, service: url.PathEscape(serviceName)}
}

func (s *Service) base(proto string) string {
	return fmt.Sprintf("/ipLoadbalancing/%s/%s", s.service, proto)
}

// ---------------------------------------------------------------------------
// Service info
// ---------------------------------------------------------------------------

// Info mirrors the top-level /ipLoadbalancing/{serviceName} object. Only the
// fields the operator needs are modelled.
type Info struct {
	// IPLoadbalancing is the public IP address (v4 or v6) of the load balancer.
	IPLoadbalancing string `json:"ipLoadbalancing,omitempty"`
	// IPv4 is the IPv4 address of the load balancer, when assigned.
	IPv4 string `json:"ipv4,omitempty"`
	// IPv6 is the IPv6 address of the load balancer, when assigned.
	IPv6        string   `json:"ipv6,omitempty"`
	Zone        []string `json:"zone,omitempty"`
	State       string   `json:"state,omitempty"`
	DisplayName string   `json:"displayName,omitempty"`
}

// GetInfo returns the top-level information for the service, including its
// assigned public IP address.
func (s *Service) GetInfo() (*Info, error) {
	var info Info
	if err := s.api.Get(fmt.Sprintf("/ipLoadbalancing/%s", s.service), &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// ListVrackNetworks returns the ids of the vRack private networks configured on
// the load balancer. A vRack-attached service requires farms to reference one
// of these via vrackNetworkId.
func (s *Service) ListVrackNetworks() ([]int64, error) {
	var ids []int64
	err := s.api.Get(fmt.Sprintf("/ipLoadbalancing/%s/vrack/network", s.service), &ids)
	return ids, err
}

// IP returns the load balancer's public IP address, preferring the canonical
// ipLoadbalancing field and falling back to the explicit v4/v6 fields.
func (i *Info) IP() string {
	switch {
	case i.IPLoadbalancing != "":
		return i.IPLoadbalancing
	case i.IPv4 != "":
		return i.IPv4
	default:
		return i.IPv6
	}
}

// ---------------------------------------------------------------------------
// Farm
// ---------------------------------------------------------------------------

// Farm mirrors the http/tcp farm object. Only fields common to both protocols
// are modelled.
type Farm struct {
	FarmID         int64  `json:"farmId,omitempty"`
	DisplayName    string `json:"displayName,omitempty"`
	Zone           string `json:"zone,omitempty"`
	Balance        string `json:"balance,omitempty"`
	Stickiness     string `json:"stickiness,omitempty"`
	Port           *int   `json:"port,omitempty"`
	VrackNetworkID *int64 `json:"vrackNetworkId,omitempty"`
	Probe          *Probe `json:"probe,omitempty"`
}

// Probe mirrors the farm health-check object.
type Probe struct {
	Type     string `json:"type,omitempty"`
	Port     *int   `json:"port,omitempty"`
	Interval *int   `json:"interval,omitempty"`
	Method   string `json:"method,omitempty"`
	URL      string `json:"url,omitempty"`
	Match    string `json:"match,omitempty"`
	Pattern  string `json:"pattern,omitempty"`
}

func (s *Service) ListFarms(proto string) ([]int64, error) {
	var ids []int64
	err := s.api.Get(s.base(proto)+"/farm", &ids)
	return ids, err
}

func (s *Service) GetFarm(proto string, id int64) (*Farm, error) {
	var f Farm
	if err := s.api.Get(fmt.Sprintf("%s/farm/%d", s.base(proto), id), &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func (s *Service) CreateFarm(proto string, f *Farm) (*Farm, error) {
	var out Farm
	if err := s.api.Post(s.base(proto)+"/farm", f, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) UpdateFarm(proto string, id int64, f *Farm) error {
	return s.api.Put(fmt.Sprintf("%s/farm/%d", s.base(proto), id), f, nil)
}

func (s *Service) DeleteFarm(proto string, id int64) error {
	return s.api.Delete(fmt.Sprintf("%s/farm/%d", s.base(proto), id), nil)
}

// ---------------------------------------------------------------------------
// Farm server
// ---------------------------------------------------------------------------

// Server mirrors a farm server member.
type Server struct {
	ServerID    int64  `json:"serverId,omitempty"`
	Address     string `json:"address,omitempty"`
	Port        *int   `json:"port,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Weight      *int   `json:"weight,omitempty"`
	Status      string `json:"status,omitempty"`
	Backup      *bool  `json:"backup,omitempty"`
	SSL         *bool  `json:"ssl,omitempty"`
	Probe       *bool  `json:"probe,omitempty"`
}

func (s *Service) serverBase(proto string, farmID int64) string {
	return fmt.Sprintf("%s/farm/%d/server", s.base(proto), farmID)
}

func (s *Service) ListServers(proto string, farmID int64) ([]int64, error) {
	var ids []int64
	err := s.api.Get(s.serverBase(proto, farmID), &ids)
	return ids, err
}

func (s *Service) GetServer(proto string, farmID, serverID int64) (*Server, error) {
	var srv Server
	if err := s.api.Get(fmt.Sprintf("%s/%d", s.serverBase(proto, farmID), serverID), &srv); err != nil {
		return nil, err
	}
	return &srv, nil
}

func (s *Service) CreateServer(proto string, farmID int64, srv *Server) (*Server, error) {
	var out Server
	if err := s.api.Post(s.serverBase(proto, farmID), srv, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) UpdateServer(proto string, farmID, serverID int64, srv *Server) error {
	return s.api.Put(fmt.Sprintf("%s/%d", s.serverBase(proto, farmID), serverID), srv, nil)
}

func (s *Service) DeleteServer(proto string, farmID, serverID int64) error {
	return s.api.Delete(fmt.Sprintf("%s/%d", s.serverBase(proto, farmID), serverID), nil)
}

// ---------------------------------------------------------------------------
// Frontend
// ---------------------------------------------------------------------------

// Frontend mirrors the http/tcp/udp frontend object. SSL/route-related fields
// only apply to http frontends.
type Frontend struct {
	FrontendID       int64    `json:"frontendId,omitempty"`
	DisplayName      string   `json:"displayName,omitempty"`
	Zone             string   `json:"zone,omitempty"`
	Port             string   `json:"port,omitempty"`
	DefaultFarmID    *int64   `json:"defaultFarmId,omitempty"`
	SSL              *bool    `json:"ssl,omitempty"`
	DefaultSslID     *int64   `json:"defaultSslId,omitempty"`
	ForceSni         *bool    `json:"forceSni,omitempty"`
	HSTS             *bool    `json:"hsts,omitempty"`
	RedirectLocation string   `json:"redirectLocation,omitempty"`
	Disabled         *bool    `json:"disabled,omitempty"`
	AllowedSource    []string `json:"allowedSource,omitempty"`
	DeniedSource     []string `json:"deniedSource,omitempty"`
}

func (s *Service) ListFrontends(proto string) ([]int64, error) {
	var ids []int64
	err := s.api.Get(s.base(proto)+"/frontend", &ids)
	return ids, err
}

func (s *Service) GetFrontend(proto string, id int64) (*Frontend, error) {
	var f Frontend
	if err := s.api.Get(fmt.Sprintf("%s/frontend/%d", s.base(proto), id), &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func (s *Service) CreateFrontend(proto string, f *Frontend) (*Frontend, error) {
	var out Frontend
	if err := s.api.Post(s.base(proto)+"/frontend", f, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) UpdateFrontend(proto string, id int64, f *Frontend) error {
	return s.api.Put(fmt.Sprintf("%s/frontend/%d", s.base(proto), id), f, nil)
}

func (s *Service) DeleteFrontend(proto string, id int64) error {
	return s.api.Delete(fmt.Sprintf("%s/frontend/%d", s.base(proto), id), nil)
}

// ---------------------------------------------------------------------------
// HTTP route (and its rules)
// ---------------------------------------------------------------------------

// Route mirrors an http route: an action run when all its rules match. Rules
// are managed as a sub-resource (they are read-only on the route object).
type Route struct {
	RouteID     int64       `json:"routeId,omitempty"`
	DisplayName string      `json:"displayName,omitempty"`
	FrontendID  *int64      `json:"frontendId,omitempty"`
	Weight      *int        `json:"weight,omitempty"`
	Action      RouteAction `json:"action"`
}

// RouteAction is the action triggered by a Route.
type RouteAction struct {
	Type   string `json:"type,omitempty"`
	Target string `json:"target,omitempty"`
	Status *int   `json:"status,omitempty"`
}

// RouteRule mirrors a single rule of an http route.
type RouteRule struct {
	RuleID      int64  `json:"ruleId,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Field       string `json:"field,omitempty"`
	Match       string `json:"match,omitempty"`
	Pattern     string `json:"pattern,omitempty"`
	SubField    string `json:"subField,omitempty"`
	Negate      *bool  `json:"negate,omitempty"`
}

// routeBase is the http route collection (routes are http-only).
func (s *Service) routeBase() string {
	return fmt.Sprintf("/ipLoadbalancing/%s/http/route", s.service)
}

func (s *Service) ListRoutes() ([]int64, error) {
	var ids []int64
	err := s.api.Get(s.routeBase(), &ids)
	return ids, err
}

func (s *Service) GetRoute(id int64) (*Route, error) {
	var r Route
	if err := s.api.Get(fmt.Sprintf("%s/%d", s.routeBase(), id), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Service) CreateRoute(r *Route) (*Route, error) {
	var out Route
	if err := s.api.Post(s.routeBase(), r, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) UpdateRoute(id int64, r *Route) error {
	return s.api.Put(fmt.Sprintf("%s/%d", s.routeBase(), id), r, nil)
}

func (s *Service) DeleteRoute(id int64) error {
	return s.api.Delete(fmt.Sprintf("%s/%d", s.routeBase(), id), nil)
}

func (s *Service) ListRouteRules(routeID int64) ([]int64, error) {
	var ids []int64
	err := s.api.Get(fmt.Sprintf("%s/%d/rule", s.routeBase(), routeID), &ids)
	return ids, err
}

func (s *Service) GetRouteRule(routeID, ruleID int64) (*RouteRule, error) {
	var rule RouteRule
	if err := s.api.Get(fmt.Sprintf("%s/%d/rule/%d", s.routeBase(), routeID, ruleID), &rule); err != nil {
		return nil, err
	}
	return &rule, nil
}

func (s *Service) CreateRouteRule(routeID int64, rule *RouteRule) (*RouteRule, error) {
	var out RouteRule
	if err := s.api.Post(fmt.Sprintf("%s/%d/rule", s.routeBase(), routeID), rule, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) DeleteRouteRule(routeID, ruleID int64) error {
	return s.api.Delete(fmt.Sprintf("%s/%d/rule/%d", s.routeBase(), routeID, ruleID), nil)
}

// ---------------------------------------------------------------------------
// SSL certificate
// ---------------------------------------------------------------------------

// Certificate mirrors the ssl object. Certificate/Key/Chain are write-only
// inputs at creation time.
type Certificate struct {
	ID          int64    `json:"id,omitempty"`
	DisplayName string   `json:"displayName,omitempty"`
	Certificate string   `json:"certificate,omitempty"`
	Key         string   `json:"key,omitempty"`
	Chain       string   `json:"chain,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	Serial      string   `json:"serial,omitempty"`
	Subject     string   `json:"subject,omitempty"`
	San         []string `json:"san,omitempty"`
	Type        string   `json:"type,omitempty"`
}

func (s *Service) ListCertificates() ([]int64, error) {
	var ids []int64
	err := s.api.Get(fmt.Sprintf("/ipLoadbalancing/%s/ssl", s.service), &ids)
	return ids, err
}

func (s *Service) GetCertificate(id int64) (*Certificate, error) {
	var c Certificate
	if err := s.api.Get(fmt.Sprintf("/ipLoadbalancing/%s/ssl/%d", s.service, id), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Service) CreateCertificate(c *Certificate) (*Certificate, error) {
	var out Certificate
	if err := s.api.Post(fmt.Sprintf("/ipLoadbalancing/%s/ssl", s.service), c, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) DeleteCertificate(id int64) error {
	return s.api.Delete(fmt.Sprintf("/ipLoadbalancing/%s/ssl/%d", s.service, id), nil)
}

// RequestFreeCertificate asks OVHcloud to issue and manage a free, auto-renewed
// Let's Encrypt certificate covering the given FQDN(s). Issuance is asynchronous
// (the FQDNs must resolve to the load balancer for ACME validation); the issued
// certificate later appears in the ssl list.
func (s *Service) RequestFreeCertificate(fqdns []string) error {
	body := map[string]any{"fqdn": fqdns}
	return s.api.Post(fmt.Sprintf("/ipLoadbalancing/%s/freeCertificate", s.service), body, nil)
}

// ---------------------------------------------------------------------------
// Apply / pending changes (the two-phase commit)
// ---------------------------------------------------------------------------

// Zone mirrors a pendingChanges entry.
type Zone struct {
	Zone string `json:"zone"`
	// Number is the count of staged-but-unapplied changes in this zone. The
	// OVHcloud API names this field "number".
	Number int `json:"number"`
}

// PendingChanges returns the per-zone pending change list. A non-empty result
// means a Refresh is required to apply staged configuration.
func (s *Service) PendingChanges() ([]Zone, error) {
	var zones []Zone
	err := s.api.Get(fmt.Sprintf("/ipLoadbalancing/%s/pendingChanges", s.service), &zones)
	return zones, err
}

// HasPendingChanges reports whether any zone has staged-but-unapplied changes.
func (s *Service) HasPendingChanges() (bool, error) {
	zones, err := s.PendingChanges()
	if err != nil {
		return false, err
	}
	for _, z := range zones {
		if z.Number > 0 {
			return true, nil
		}
	}
	return false, nil
}

// Refresh applies the staged configuration to the load balancer.
func (s *Service) Refresh() error {
	return s.api.Post(fmt.Sprintf("/ipLoadbalancing/%s/refresh", s.service), nil, nil)
}
