package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"

	"gopkg.in/jcmturner/gokrb5.v7/keytab"
)

type KDCAPIServer struct {
	kadmin   *KAdminClient
	endpoint string
}

type KDCRequestAddPrincipal struct {
	PrincipalsRaw string       `json:"principals_raw"`
	Principals    []KPrincipal `json:"principals"`
	Secret        string       `json:"secret"`
	Binary        *bool        `json:"binary"`
}

type KDCRequestListPrincipals struct {
	Filter string `json:"filter"`
	Secret string `json:"secret,omitempty`
	Binary *bool  `json:"binary"`
}

type KDCListPrincipalsResponse struct {
	List     []KPrincipal `json:"list,omitempty"`
	Checksum string       `json:"checksum,omitempty"`
}

type KDCCheckStatus struct {
	Pass     bool   `json:"pass"`
	Reason   string `json:"reason,omitempty"`
	Checksum string `json:"checksum,omitempty"`
}

type KDCResponse struct {
	Status     string                     `json:"status"`
	Error      string                     `json:"error,omitempty"`
	Principals *KDCListPrincipalsResponse `json:"principals,omitempty"`
	Check      *KDCCheckStatus            `json:"check,omitempty"`
}

/**
 * Sortable list of principals
 */
type KPrincipalList []KPrincipal

func (p KPrincipalList) Len() int {
	return len(p)
}
func (p KPrincipalList) Less(i, j int) bool {
	return p[i].Full() < p[j].Full()
}
func (p KPrincipalList) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}

type KTKey struct {
	Type         int32
	HashContents string
}

/**
 * Sortable list of encryption keys
 */
type KTKeyList []KTKey

func (p KTKeyList) Len() int {
	return len(p)
}
func (p KTKeyList) Less(i, j int) bool {
	return p[i].Type < p[j].Type
}
func (p KTKeyList) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}

func createKDCAPIServer(kadmin *KAdminClient, port string, host string) *KDCAPIServer {
	inst := &KDCAPIServer{
		kadmin:   kadmin,
		endpoint: fmt.Sprintf("%s:%s", host, port),
	}

	// Register the static API server
	fs := http.FileServer(http.Dir("static/"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Welcome to KDC Admin")
	})

	// Register the app routes
	http.HandleFunc("/api/principals", inst.handlePrincipals)
	http.HandleFunc("/api/check", inst.handleCheckPrincipals)
	return inst
}

func (s *KDCAPIServer) Start() {
	log.Printf("Listening on %s\n", s.endpoint)
	http.ListenAndServe(s.endpoint, nil)
}

/**
 * Respond with an error, formatting it according to the request type
 */
func (s *KDCAPIServer) replyReject(rw http.ResponseWriter, req *http.Request, format string, args ...interface{}) {
	contentType := req.Header.Get("Content-Type")
	if contentType == "application/json" {
		// If the body contains a JSON payload, respond with JSON payload
		resp := KDCResponse{
			"error",
			fmt.Sprintf(format, args...),
			nil,
			nil,
		}

		js, err := json.Marshal(resp)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		rw.Header().Set("Content-Type", "application/json")
		rw.Write(js)

	} else {
		// Otherwise respond with plain text
		rw.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(rw, "errr: "+format, args...)
	}
}

func (s *KDCAPIServer) replySuccess(rw http.ResponseWriter, req *http.Request, data interface{}) {
	contentType := req.Header.Get("Content-Type")

	if contentType == "application/json" {
		// If the body contains a JSON payload, respond with JSON payload
		resp := KDCResponse{
			"ok", "", nil, nil,
		}

		if data != nil {
			switch x := data.(type) {
			case *KDCListPrincipalsResponse:
				resp.Principals = x
			case *KDCCheckStatus:
				resp.Check = x
			}
		}

		js, err := json.Marshal(resp)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		rw.Header().Set("Content-Type", "application/json")
		rw.Write(js)

	} else {
		// Otherwise respond with plain text
		rw.Header().Set("Content-Type", "text/plain")
		if data != nil {
			switch x := data.(type) {
			case *KDCListPrincipalsResponse:
				for _, v := range x.List {
					fmt.Fprintln(rw, v.String())
				}
			case *KDCCheckStatus:
				if x.Pass {
					fmt.Fprintln(rw, "pass")
				} else {
					fmt.Fprintf(rw, "fail,%s\n", x.Reason)
				}
			}
		}

		fmt.Fprintf(rw, "ok")
	}
}

/**
 * getPrincipalsChecksum calculates a checksum with the contents of the keys
 * of all the principals given, as found in the keytab given.
 */
func getPrincipalsChecksum(keytabBytes []byte, principals []KPrincipal) (string, error) {
	var kt keytab.Keytab
	var principalKeys map[string]KTKeyList = make(map[string]KTKeyList)
	var sortedPrincipals KPrincipalList = nil
	var csumContents string = ""

	// Parse the keytab contents and create a sorted list of keys for each principal
	err := kt.Unmarshal(keytabBytes)
	if err != nil {
		return "", fmt.Errorf(`Unable to parse keytab contents: %s`, err.Error())
	}
	for _, entry := range kt.Entries {
		var p KPrincipal
		p.Realm = entry.Principal.Realm
		if len(entry.Principal.Components) > 0 {
			p.Primary = entry.Principal.Components[0]
		}
		if len(entry.Principal.Components) > 1 {
			p.Instance = entry.Principal.Components[1]
		}

		var k KTKey
		k.Type = entry.Key.KeyType
		k.HashContents = fmt.Sprintf("%d:%x", entry.Key.KeyType, entry.Key.KeyValue)

		var list KTKeyList = nil
		if l, ok := principalKeys[p.Full()]; ok {
			list = l
		} else {
			list = nil
		}

		list = append(list, k)
		sort.Sort(list)
		principalKeys[p.Full()] = list
	}

	// Create a sorted list of principals and calculate a unique checksum
	// in the order they appear
	for _, principal := range principals {
		sortedPrincipals = append(sortedPrincipals, principal)
	}
	sort.Sort(sortedPrincipals)
	for _, principal := range sortedPrincipals {
		// If the entry was not found, raise an error since in the previous steps
		// we made sure that the principal should be included in the keytab
		keyList, ok := principalKeys[principal.Full()]
		if !ok {
			return "", fmt.Errorf("Could not located principal '%s' in the parsed keytab", principal.Full())
		}

		// Include the keys of this principal
		for _, key := range keyList {
			if csumContents != "" {
				csumContents += ","
			}
			csumContents += key.HashContents
		}
	}

	// Hash the checksum
	sum := sha256.Sum256([]byte(csumContents))
	return fmt.Sprintf("%x", sum), nil
}

/**
 * CRUD Entrypoint for /principals endpoint
 */
func (s *KDCAPIServer) handlePrincipals(rw http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case "POST":
		s.handleAddPrincipal(rw, req)
	case "GET":
		s.handleListPrincipals(rw, req)
	case "DELETE":
		s.handleDeletePrincipals(rw, req)
	default:
		s.replyReject(rw, req, `Accepting only POST/GET/DELETE requests on this endpoint`)
	}
}

/**
 * Add one or more principals to the KDC server, and create the respective
 * service secret name
 */
func (s *KDCAPIServer) handleAddPrincipal(rw http.ResponseWriter, req *http.Request) {
	var apiReq KDCRequestAddPrincipal

	// Check if we are parsing JSON or plain text
	contentType := req.Header.Get("Content-Type")
	if contentType == "application/json" {

		// We are expeting all the interesting data to be in the payload
		dec := json.NewDecoder(req.Body)
		if err := dec.Decode(&apiReq); err == io.EOF {
			s.replyReject(rw, req, `Could not decode input`)
			return
		} else if err != nil {
			s.replyReject(rw, req, `Unable to parse request: %s`, err.Error())
			return
		}

		// If we were given a raw principals list, parse it now
		if apiReq.PrincipalsRaw != "" {
			principalsList, err := ParsePrincipalsFrom(strings.NewReader(apiReq.PrincipalsRaw))
			if err != nil {
				s.replyReject(rw, req, `unable to parse principals: %s`, err.Error())
				return
			}
			apiReq.Principals = principalsList
		}

	} else {

		// Otherwise we expect the secret name to be on the request
		secretName, ok := req.URL.Query()["secret"]
		if !ok || len(secretName[0]) < 1 {
			s.replyReject(rw, req, `missing 'secret=' argument`)
			return
		}
		useBinary, ok := req.URL.Query()["binary"]
		if ok && len(secretName[0]) > 0 {
			useBinaryFlag := useBinary[0] == "1"
			apiReq.Binary = &useBinaryFlag
		}

		// Parse principals body as plaintext
		principalsList, err := ParsePrincipalsFrom(req.Body)
		if err != nil {
			s.replyReject(rw, req, `unable to parse principals: %s`, err.Error())
			return
		}

		// Populate API request struct
		apiReq.Secret = secretName[0]
		apiReq.Principals = principalsList

	}

	if len(apiReq.Principals) == 0 {
		s.replyReject(rw, req, `given an empty list of principals`)
		return
	}

	// Since our auth token can expire at any time, we are re-connecting on
	// DC/OS on every request. Since we are not expecting any serious request
	// rate on this endpoint,  and since the log-in procedure is quite fast
	// we should be OK
	dclient, err := createDCOSClientFromEnvironment()
	if err != nil {
		s.replyReject(rw, req, `Unable to connect to DC/OS: %s`, err.Error())
		return
	}

	err = s.kadmin.AddMissingPrincipals(apiReq.Principals)
	if err != nil {
		s.replyReject(rw, req, `Unable to add principals: %s`, err.Error())
		return
	}

	keytab, err := s.kadmin.GetKeytabForPrincipals(apiReq.Principals)
	if err != nil {
		s.replyReject(rw, req, `Unable to export keytab: %s`, err.Error())
		return
	}

	useBinary := false
	if apiReq.Binary != nil {
		useBinary = *apiReq.Binary
	}

	err = CreateKeytabSecret(dclient, apiReq.Secret, keytab, useBinary)
	if err != nil {
		s.replyReject(rw, req, `Unable to upload to secret store: %s`, err.Error())
		return
	}

	// We are done
	s.replySuccess(rw, req, nil)
}

/**
 * Enumerates the installed principals that match a given wildcard
 */
func (s *KDCAPIServer) handleListPrincipals(rw http.ResponseWriter, req *http.Request) {
	var resp KDCListPrincipalsResponse
	filterExpr := KDCRequestListPrincipals{"*", "", nil}

	// Check if we are parsing JSON or plain text
	contentType := req.Header.Get("Content-Type")
	if contentType == "application/json" {
		// We are expeting all the interesting data to be in the payload
		dec := json.NewDecoder(req.Body)
		if err := dec.Decode(&filterExpr); err == io.EOF {
			s.replyReject(rw, req, `Could not decode input`)
			return
		} else if err != nil {
			s.replyReject(rw, req, `Unable to parse request: %s`, err.Error())
			return
		}

	} else {
		// Otherwise we expect the secret name to be on the request
		filterArg, ok := req.URL.Query()["filter"]
		if ok && len(filterArg[0]) > 0 {
			filterExpr.Filter = filterArg[0]
		}
		secretName, ok := req.URL.Query()["secret"]
		if !ok || len(secretName[0]) < 1 {
			filterExpr.Secret = secretName[0]
		}
		useBinary, ok := req.URL.Query()["binary"]
		if ok && len(secretName[0]) > 0 {
			useBinaryFlag := useBinary[0] == "1"
			filterExpr.Binary = &useBinaryFlag
		}
	}

	list, err := s.kadmin.ListPrincipals(filterExpr.Filter)
	if err != nil {
		s.replyReject(rw, req, `Unable to list principals: %s`, err.Error())
		return
	}

	// If there was a secret argument in the query, strip-out principals
	// not present in the secret file given
	if filterExpr.Secret != "" {
		// Since our auth token can expire at any time, we are re-connecting on
		// DC/OS on every request. Since we are not expecting any serious request
		// rate on this endpoint,  and since the log-in procedure is quite fast
		// we should be OK
		dclient, err := createDCOSClientFromEnvironment()
		if err != nil {
			s.replyReject(rw, req, `Unable to connect to DC/OS: %s`, err.Error())
			return
		}

		useBinary := false
		if filterExpr.Binary != nil {
			useBinary = *filterExpr.Binary
		}

		ktBytes, err := GetKeytabSecret(dclient, filterExpr.Secret, useBinary)
		if err != nil {
			s.replyReject(rw, req, `Unable to read the keytab secret: %s`, err.Error())
			return
		}

		if ktBytes == nil {
			s.replyReject(rw, req, `The secret was empty`)
			return
		}

		// Filter-out missing principals
		var newList []KPrincipal = nil
		for _, principal := range list {
			ok, err := s.kadmin.HasPrincipalInKeytab(ktBytes, &principal)
			if err != nil {
				s.replyReject(rw, req, `Unable to check if principal %s exists in keytab: %s`, principal.Full(), err.Error())
				return
			}
			if ok {
				newList = append(newList, principal)
			}
		}
		list = newList

		csum, err := getPrincipalsChecksum(ktBytes, list)
		if err != nil {
			s.replyReject(rw, req, err.Error())
			return
		}
		resp.Checksum = csum
	}

	resp.List = list
	s.replySuccess(rw, req, &resp)
}

/**
 * Deletes the secret from DC/OS and revokes the principals from KDC
 */
func (s *KDCAPIServer) handleDeletePrincipals(rw http.ResponseWriter, req *http.Request) {
	var apiReq KDCRequestAddPrincipal

	// Check if we are parsing JSON or plain text
	contentType := req.Header.Get("Content-Type")
	if contentType == "application/json" {

		// We are expeting all the interesting data to be in the payload
		dec := json.NewDecoder(req.Body)
		if err := dec.Decode(&apiReq); err == io.EOF {
			s.replyReject(rw, req, `Could not decode input`)
			return
		} else if err != nil {
			s.replyReject(rw, req, `Unable to parse request: %s`, err.Error())
			return
		}

		// If we were given a raw principals list, parse it now
		if apiReq.PrincipalsRaw != "" {
			principalsList, err := ParsePrincipalsFrom(strings.NewReader(apiReq.PrincipalsRaw))
			if err != nil {
				s.replyReject(rw, req, `unable to parse principals: %s`, err.Error())
				return
			}
			apiReq.Principals = principalsList
		}

	} else {

		// Otherwise we expect the secret name to be on the request
		secretName, ok := req.URL.Query()["secret"]
		if !ok || len(secretName[0]) < 1 {
			s.replyReject(rw, req, `missing 'secret=' argument`)
			return
		}
		useBinary, ok := req.URL.Query()["binary"]
		if ok && len(secretName[0]) > 0 {
			useBinaryFlag := useBinary[0] == "1"
			apiReq.Binary = &useBinaryFlag
		}

		// Parse principals body as plaintext
		principalsList, err := ParsePrincipalsFrom(req.Body)
		if err != nil {
			s.replyReject(rw, req, `unable to parse principals: %s`, err.Error())
			return
		}

		// Populate API request struct
		apiReq.Secret = secretName[0]
		apiReq.Principals = principalsList

	}

	// Check if we were given an empty string
	if len(apiReq.Principals) == 0 {
		s.replyReject(rw, req, `given an empty list of principals`)
		return
	}

	useBinary := false
	if apiReq.Binary != nil {
		useBinary = *apiReq.Binary
	}

	// Since our auth token can expire at any time, we are re-connecting on
	// DC/OS on every request. Since we are not expecting any serious request
	// rate on this endpoint,  and since the log-in procedure is quite fast
	// we should be OK
	dclient, err := createDCOSClientFromEnvironment()
	if err != nil {
		s.replyReject(rw, req, `Unable to connect to DC/OS: %s`, err.Error())
		return
	}

	err = DeleteKeytabSecret(dclient, apiReq.Secret, useBinary)
	if err != nil {
		s.replyReject(rw, req, `Unable to delete secret: %s`, err.Error())
		return
	}

	err = s.kadmin.DeletePrincipals(apiReq.Principals)
	if err != nil {
		s.replyReject(rw, req, `Unable to delete principals: %s`, err.Error())
		return
	}

	s.replySuccess(rw, req, nil)
}

/**
 * Checks if all the requested principals exists in the respective secret
 */
func (s *KDCAPIServer) handleCheckPrincipals(rw http.ResponseWriter, req *http.Request) {
	var apiReq KDCRequestAddPrincipal

	// We accept only POST
	if req.Method != "POST" {
		s.replyReject(rw, req, `Accepting only POST requests on this endpoint`)
		return
	}

	// Check if we are parsing JSON or plain text
	contentType := req.Header.Get("Content-Type")
	if contentType == "application/json" {

		// We are expecting all the interesting data to be in the payload
		dec := json.NewDecoder(req.Body)
		if err := dec.Decode(&apiReq); err == io.EOF {
			s.replyReject(rw, req, `Could not decode input`)
			return
		} else if err != nil {
			s.replyReject(rw, req, `Unable to parse request: %s`, err.Error())
			return
		}

		// If we were given a raw principals list, parse it now
		if apiReq.PrincipalsRaw != "" {
			principalsList, err := ParsePrincipalsFrom(strings.NewReader(apiReq.PrincipalsRaw))
			if err != nil {
				s.replyReject(rw, req, `unable to parse principals: %s`, err.Error())
				return
			}
			apiReq.Principals = principalsList
		}

	} else {

		// In the plaintext case, we expect the secret name to be on the request
		secretName, ok := req.URL.Query()["secret"]
		if !ok || len(secretName[0]) < 1 {
			s.replyReject(rw, req, `missing 'secret=' argument`)
			return
		}
		useBinary, ok := req.URL.Query()["binary"]
		if ok && len(secretName[0]) > 0 {
			useBinaryFlag := useBinary[0] == "1"
			apiReq.Binary = &useBinaryFlag
		}

		// Parse principals body as plaintext
		principalsList, err := ParsePrincipalsFrom(req.Body)
		if err != nil {
			s.replyReject(rw, req, `unable to parse principals: %s`, err.Error())
			return
		}

		apiReq.Secret = secretName[0]
		apiReq.Principals = principalsList

	}

	if len(apiReq.Principals) == 0 {
		s.replyReject(rw, req, `given an empty list of principals`)
		return
	}

	// Before continuing with validating the secret, make sure that all the
	// principals are present in KDC
	for _, principal := range apiReq.Principals {
		ok, err := s.kadmin.HasPrincipal(principal)
		if err != nil {
			s.replyReject(rw, req, `Unable to check if principal %s exists: %s`, principal.Full(), err.Error())
			return
		}
		if !ok {
			// We don't have a required principal -> check failed
			s.replySuccess(rw, req, &KDCCheckStatus{
				false, fmt.Sprintf("Principal '%s' does not exist in kerberos", principal.Full()), "",
			})
			return
		}
	}

	// Since our auth token can expire at any time, we are re-connecting on
	// DC/OS on every request. Since we are not expecting any serious request
	// rate on this endpoint,  and since the log-in procedure is quite fast
	// we should be OK
	dclient, err := createDCOSClientFromEnvironment()
	if err != nil {
		s.replyReject(rw, req, `Unable to connect to DC/OS: %s`, err.Error())
		return
	}

	useBinary := false
	if apiReq.Binary != nil {
		useBinary = *apiReq.Binary
	}

	ktBytes, err := GetKeytabSecret(dclient, apiReq.Secret, useBinary)
	if err != nil {
		s.replyReject(rw, req, `Unable to read the keytab secret: %s`, err.Error())
		return
	}

	// If the secret is empty, fail the check
	if ktBytes == nil {
		s.replySuccess(rw, req, &KDCCheckStatus{
			false, fmt.Sprintf("Secret '%s' does not exist", apiReq.Secret), "",
		})
		return
	}

	// Check if the requested principals do not exist
	for _, principal := range apiReq.Principals {
		ok, err := s.kadmin.HasPrincipalInKeytab(ktBytes, &principal)
		if err != nil {
			s.replyReject(rw, req, `Unable to check if principal %s exists in keytab: %s`, principal.Full(), err.Error())
			return
		}
		if !ok {
			// We don't have a required principal in the keytab -> check failed
			s.replySuccess(rw, req, &KDCCheckStatus{
				false, fmt.Sprintf("Principal '%s' does not exist in keytab", principal.Full()), "",
			})
			return
		}
	}

	csum, err := getPrincipalsChecksum(ktBytes, apiReq.Principals)
	if err != nil {
		s.replyReject(rw, req, err.Error())
		return
	}

	s.replySuccess(rw, req, &KDCCheckStatus{true, "", csum})
}
