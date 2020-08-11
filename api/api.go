// Copyright 2017 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/golang/glog"
	"github.com/gorilla/mux"
	"github.com/tsuru/kubernetes-router/router"
)

// RouterAPI implements Tsuru HTTP router API
type RouterAPI struct {
	DefaultMode     string
	IngressServices map[string]router.Service
}

// Routes returns an mux for the API routes
func (a *RouterAPI) Routes() *mux.Router {
	r := mux.NewRouter()
	a.registerRoutes(r.PathPrefix("/api").Subrouter())
	a.registerRoutes(r.PathPrefix("/api/{mode}").Subrouter())
	return r
}

func (a *RouterAPI) registerRoutes(r *mux.Router) {
	r.Handle("/backend/{name}", handler(a.getBackend)).Methods(http.MethodGet)
	r.Handle("/backend/{name}", handler(a.addBackend)).Methods(http.MethodPost)
	r.Handle("/backend/{name}", handler(a.updateBackend)).Methods(http.MethodPut)
	r.Handle("/backend/{name}", handler(a.removeBackend)).Methods(http.MethodDelete)
	r.Handle("/backend/{name}/routes", handler(a.getRoutes)).Methods(http.MethodGet)
	r.Handle("/backend/{name}/routes", handler(a.addRoutes)).Methods(http.MethodPost)
	r.Handle("/backend/{name}/routes/remove", handler(a.removeRoutes)).Methods(http.MethodPost)
	r.Handle("/backend/{name}/swap", handler(a.swap)).Methods(http.MethodPost)

	r.Handle("/info", handler(a.info)).Methods(http.MethodGet)

	// TLS
	r.Handle("/backend/{name}/certificate/{certname}", handler(a.addCertificate)).Methods(http.MethodPut)
	r.Handle("/backend/{name}/certificate/{certname}", handler(a.getCertificate)).Methods(http.MethodGet)
	r.Handle("/backend/{name}/certificate/{certname}", handler(a.removeCertificate)).Methods(http.MethodDelete)

	// CNAME
	r.Handle("/backend/{name}/cname/{cname}", handler(a.setCname)).Methods(http.MethodPost)
	r.Handle("/backend/{name}/cname", handler(a.getCnames)).Methods(http.MethodGet)
	r.Handle("/backend/{name}/cname/{cname}", handler(a.unsetCname)).Methods(http.MethodDelete)

	// Supports
	r.Handle("/support/tls", handler(a.supportTLS)).Methods(http.MethodGet)
	r.Handle("/support/cname", handler(a.supportCNAME)).Methods(http.MethodGet)
	r.Handle("/support/info", handler(func(w http.ResponseWriter, r *http.Request) error {
		w.WriteHeader(http.StatusOK)
		return nil
	})).Methods(http.MethodGet)
	r.Handle("/support/prefix", handler(func(w http.ResponseWriter, r *http.Request) error {
		w.WriteHeader(http.StatusOK)
		return nil
	})).Methods(http.MethodGet)
}

func (a *RouterAPI) ingressService(mode string) (router.Service, error) {
	if mode == "" {
		mode = a.DefaultMode
	}
	svc, ok := a.IngressServices[mode]
	if !ok {
		return nil, httpError{Status: http.StatusNotFound}
	}
	return svc, nil
}

func instanceID(r *http.Request) router.InstanceID {
	vars := mux.Vars(r)
	return router.InstanceID{
		AppName:      vars["name"],
		InstanceName: r.Header.Get("X-Router-Instance"),
	}
}

// getBackend returns the address for the load balancer registered in
// the ingress by a ingress controller
func (a *RouterAPI) getBackend(w http.ResponseWriter, r *http.Request) error {
	vars := mux.Vars(r)
	svc, err := a.ingressService(vars["mode"])
	if err != nil {
		return err
	}
	addrs, err := svc.GetAddresses(instanceID(r))
	if err != nil {
		return err
	}
	type getBackendResponse struct {
		Address   string   `json:"address"`
		Addresses []string `json:"addresses"`
	}
	rsp := getBackendResponse{
		Addresses: addrs,
	}
	if len(addrs) > 0 {
		rsp.Address = addrs[0]
	}
	return json.NewEncoder(w).Encode(rsp)
}

// addBackend creates a Ingress for a given app configuration pointing
// to a non existent service
func (a *RouterAPI) addBackend(w http.ResponseWriter, r *http.Request) error {
	vars := mux.Vars(r)
	routerOpts := router.Opts{
		HeaderOpts: r.Header.Values("X-Router-Opt"),
	}
	err := json.NewDecoder(r.Body).Decode(&routerOpts)
	if err != nil {
		return err
	}
	if len(routerOpts.Domain) > 0 && len(routerOpts.Route) == 0 {
		routerOpts.Route = "/"
	}
	svc, err := a.ingressService(vars["mode"])
	if err != nil {
		return err
	}
	return svc.Create(instanceID(r), routerOpts)
}

// updateBackend is no-op
func (a *RouterAPI) updateBackend(w http.ResponseWriter, r *http.Request) error {
	return nil
}

// removeBackend removes the Ingress for a given app
func (a *RouterAPI) removeBackend(w http.ResponseWriter, r *http.Request) error {
	vars := mux.Vars(r)
	svc, err := a.ingressService(vars["mode"])
	if err != nil {
		return err
	}
	return svc.Remove(instanceID(r))
}

// addRoutes updates the Ingress to point to the correct service
func (a *RouterAPI) addRoutes(w http.ResponseWriter, r *http.Request) error {
	vars := mux.Vars(r)

	var routesData router.RoutesRequestData
	err := json.NewDecoder(r.Body).Decode(&routesData)
	if err != nil {
		return err
	}
	if routesData.Prefix != "" {
		// Do nothing for all prefixes, except the default one.
		return nil
	}

	svc, err := a.ingressService(vars["mode"])
	if err != nil {
		return err
	}

	return svc.Update(instanceID(r), routesData.ExtraData)
}

// removeRoutes is no-op
func (a *RouterAPI) removeRoutes(w http.ResponseWriter, r *http.Request) error {
	return nil
}

// getRoutes always returns an empty address list to force tsuru to call
// addRoutes on every routes rebuild call.
func (a *RouterAPI) getRoutes(w http.ResponseWriter, r *http.Request) error {
	type resp struct {
		Addresses []string `json:"addresses"`
	}
	return json.NewEncoder(w).Encode(resp{})
}

func (a *RouterAPI) swap(w http.ResponseWriter, r *http.Request) error {
	vars := mux.Vars(r)
	type swapReq struct {
		Target string
	}
	var req swapReq
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return errors.New("error parsing request")
	}
	if req.Target == "" {
		return httpError{Body: "empty target", Status: http.StatusBadRequest}
	}
	svc, err := a.ingressService(vars["mode"])
	if err != nil {
		return err
	}
	src := instanceID(r)
	dst := router.InstanceID{
		InstanceName: src.InstanceName,
		AppName:      req.Target,
	}
	return svc.Swap(src, dst)
}

func (a *RouterAPI) info(w http.ResponseWriter, r *http.Request) error {
	vars := mux.Vars(r)
	svc, err := a.ingressService(vars["mode"])
	if err != nil {
		return err
	}
	opts := svc.SupportedOptions()
	allOpts := router.DescribedOptions()
	info := make(map[string]string)
	for k, v := range opts {
		vv := v
		if vv == "" {
			vv = allOpts[k]
		}
		info[k] = vv
	}
	return json.NewEncoder(w).Encode(info)
}

// Healthcheck checks the health of the service
func (a *RouterAPI) Healthcheck(w http.ResponseWriter, req *http.Request) {
	var err error
	defer func() {
		if err != nil {
			glog.Errorf("failed to write healthcheck: %v", err)
		}
	}()
	var errors []string
	for mode, svc := range a.IngressServices {
		if hc, ok := svc.(router.HealthcheckableService); ok {
			if err = hc.Healthcheck(); err != nil {
				errors = append(errors, fmt.Sprintf("failed to check IngressService %v: %v", mode, err))
			}
		}
	}
	if len(errors) > 0 {
		w.WriteHeader(http.StatusInternalServerError)
		_, err = w.Write([]byte(strings.Join(errors, " - ")))
		return
	}
	_, err = w.Write([]byte("WORKING"))
}

// addCertificate Add certificate to app
func (a *RouterAPI) addCertificate(w http.ResponseWriter, r *http.Request) error {
	vars := mux.Vars(r)
	name := vars["name"]
	certName := vars["certname"]
	log.Printf("Adding on %s certificate %s", name, certName)
	cert := router.CertData{}
	err := json.NewDecoder(r.Body).Decode(&cert)
	if err != nil {
		return err
	}
	svc, err := a.ingressService(vars["mode"])
	if err != nil {
		return err
	}
	return svc.(router.ServiceTLS).AddCertificate(instanceID(r), certName, cert)
}

// getCertificate Return certificate for app
func (a *RouterAPI) getCertificate(w http.ResponseWriter, r *http.Request) error {
	vars := mux.Vars(r)
	name := vars["name"]
	certName := vars["certname"]
	log.Printf("Getting certificate %s from %s", certName, name)
	svc, err := a.ingressService(vars["mode"])
	if err != nil {
		return err
	}
	cert, err := svc.(router.ServiceTLS).GetCertificate(instanceID(r), certName)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return err
	}
	b, err := json.Marshal(&cert)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(b)
	return err
}

// removeCertificate Delete certificate for app
func (a *RouterAPI) removeCertificate(w http.ResponseWriter, r *http.Request) error {
	vars := mux.Vars(r)
	name := vars["name"]
	certName := vars["certname"]
	log.Printf("Removing certificate %s from %s", certName, name)
	svc, err := a.ingressService(vars["mode"])
	if err != nil {
		return err
	}
	err = svc.(router.ServiceTLS).RemoveCertificate(instanceID(r), certName)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
	}
	return err
}

// setCname Add CNAME to app
func (a *RouterAPI) setCname(w http.ResponseWriter, r *http.Request) error {
	vars := mux.Vars(r)
	name := vars["name"]
	cname := vars["cname"]
	log.Printf("Adding on %s CNAME %s", name, cname)
	svc, err := a.ingressService(vars["mode"])
	if err != nil {
		return err
	}
	err = svc.(router.ServiceCNAME).SetCname(instanceID(r), cname)
	if err != nil {
		if strings.Contains(err.Error(), "exists") {
			w.WriteHeader(http.StatusConflict)
		}
		w.WriteHeader(http.StatusNotFound)
	}
	return err
}

// getCnames Return CNAMEs for app
func (a *RouterAPI) getCnames(w http.ResponseWriter, r *http.Request) error {
	vars := mux.Vars(r)
	name := vars["name"]
	log.Printf("Getting CNAMEs from %s", name)
	svc, err := a.ingressService(vars["mode"])
	if err != nil {
		return err
	}
	cnames, err := svc.(router.ServiceCNAME).GetCnames(instanceID(r))
	if err != nil {
		return err
	}
	b, err := json.Marshal(&cnames)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(b)
	return err
}

// unsetCname Delete CNAME for app
func (a *RouterAPI) unsetCname(w http.ResponseWriter, r *http.Request) error {
	vars := mux.Vars(r)
	name := vars["name"]
	cname := vars["cname"]
	log.Printf("Removing CNAME %s from %s", cname, name)
	svc, err := a.ingressService(vars["mode"])
	if err != nil {
		return err
	}
	return svc.(router.ServiceCNAME).UnsetCname(instanceID(r), cname)
}

// Check for TLS Support
func (a *RouterAPI) supportTLS(w http.ResponseWriter, r *http.Request) error {
	var err error
	vars := mux.Vars(r)
	svc, err := a.ingressService(vars["mode"])
	if err != nil {
		return err
	}
	_, ok := svc.(router.ServiceTLS)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, err = w.Write([]byte("No TLS Capabilities"))
		return err
	}
	_, err = w.Write([]byte("OK"))
	return err
}

// Check for CNAME Support
func (a *RouterAPI) supportCNAME(w http.ResponseWriter, r *http.Request) error {
	var err error
	vars := mux.Vars(r)
	svc, err := a.ingressService(vars["mode"])
	if err != nil {
		return err
	}
	_, ok := svc.(router.ServiceCNAME)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, err = w.Write([]byte("No CNAME Capabilities"))
		return err
	}
	_, err = w.Write([]byte("OK"))
	return err
}
