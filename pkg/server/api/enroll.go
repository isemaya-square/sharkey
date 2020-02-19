/*-
 * Copyright 2016 Square Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package api

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/square/sharkey/pkg/server/config"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

func logHttpError(r *http.Request, w http.ResponseWriter, err error, code int, logger *logrus.Logger) {
	// Log an error response:
	// POST /enroll/example.com: 404 some message
	logger.WithFields(logrus.Fields{
		"method": r.Method,
		"url":    r.URL,
		"code":   code,
	}).WithError(err).Error("logHttpError")

	http.Error(w, err.Error(), code)
}

func (c *Api) Enroll(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	hostname := vars["hostname"]

	if !clientAuthenticated(r) {
		http.Error(w, "no client certificate provided", http.StatusUnauthorized)
		return
	}
	if !clientHostnameMatches(hostname, r) {
		http.Error(w, "hostname does not match certificate", http.StatusForbidden)
		return
	}

	cert, err := c.EnrollHost(hostname, r)
	if err != nil {
		c.logger.Error("internal error")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, _ = w.Write([]byte(cert))
}

// Read a public key off the wire
func readPubkey(r *http.Request) (ssh.PublicKey, error) {
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	pubkey, _, _, _, err := ssh.ParseAuthorizedKey(data)
	return pubkey, err
}

func encodeCert(certificate *ssh.Certificate) (string, error) {
	certString := base64.StdEncoding.EncodeToString(certificate.Marshal())
	return fmt.Sprintf("%s-cert-v01@openssh.com %s\n", certificate.Key.Type(), certString), nil
}

func (c *Api) EnrollHost(hostname string, r *http.Request) (string, error) {
	pubkey, err := readPubkey(r)
	if err != nil {
		return "", err
	}

	// Update table with host
	id, err := c.storage.RecordIssuance(ssh.HostCert, hostname, pubkey)
	if err != nil {
		return "", err
	}

	signedCert, err := c.signHost(hostname, id, pubkey)
	if err != nil {
		return "", err
	}

	return encodeCert(signedCert)
}

func clientAuthenticated(r *http.Request) bool {
	return len(r.TLS.VerifiedChains) > 0
}

func clientHostnameMatches(hostname string, r *http.Request) bool {
	conn := r.TLS
	if len(conn.VerifiedChains) == 0 {
		return false
	}
	cert := conn.VerifiedChains[0][0]
	return cert.VerifyHostname(hostname) == nil
}

func (c *Api) signHost(hostname string, serial uint64, pubkey ssh.PublicKey) (*ssh.Certificate, error) {
	principals := []string{hostname}
	if c.conf.StripSuffix != "" && strings.HasSuffix(hostname, c.conf.StripSuffix) {
		principals = append(principals, strings.TrimSuffix(hostname, c.conf.StripSuffix))
	}
	if aliases, ok := c.conf.Aliases[hostname]; ok {
		principals = append(principals, aliases...)
	}
	return c.sign(hostname, principals, serial, ssh.HostCert, pubkey)
}

func (c *Api) sign(keyId string, principals []string, serial uint64, certType uint32, pubkey ssh.PublicKey) (*ssh.Certificate, error) {
	nonce := make([]byte, 32)
	_, err := rand.Read(nonce)
	if err != nil {
		return nil, err
	}
	startTime := time.Now()
	duration, err := getDurationForCertType(c.conf, certType)
	if err != nil {
		return nil, err
	}
	endTime := startTime.Add(duration)
	template := ssh.Certificate{
		Nonce:           nonce,
		Key:             pubkey,
		Serial:          serial,
		CertType:        certType,
		KeyId:           keyId,
		ValidPrincipals: principals,
		ValidAfter:      (uint64)(startTime.Unix()),
		ValidBefore:     (uint64)(endTime.Unix()),
		Permissions:     getPermissionsForCertType(&c.conf.SSH, certType),
	}

	err = template.SignCert(rand.Reader, c.signer)
	if err != nil {
		return nil, err
	}
	return &template, nil
}

// This assumes there's an authenticating proxy which provides the user in a header, configurable.
// We identify the proxy with its TLS client cert
func proxyAuthenticated(ap *config.AuthenticatingProxy, w http.ResponseWriter, r *http.Request, logger *logrus.Logger) (string, bool) {
	if ap == nil {
		// Client certificates are not configured
		logHttpError(r, w, errors.New("client certificates are unavailable"), http.StatusNotFound, logger)
		return "", false
	}

	if !(clientAuthenticated(r) && clientHostnameMatches(ap.Hostname, r)) {
		logHttpError(r, w, fmt.Errorf("request didn't come from proxy"), http.StatusUnauthorized, logger)
		return "", false
	}

	user := r.Header.Get(ap.UsernameHeader)
	if user == "" { // Shouldn't happen
		logHttpError(r, w, errors.New("no username supplied"), http.StatusUnauthorized, logger)
		return "", false
	}

	// We've got a valid connection from the authenticating proxy.
	return user, true
}

func (c *Api) EnrollUser(w http.ResponseWriter, r *http.Request) {
	user, ok := proxyAuthenticated(c.conf.AuthenticatingProxy, w, r, c.logger)
	if !ok {
		// proxyAuthenticated sets http status & logs message
		return
	}

	pk, err := readPubkey(r)
	if err != nil {
		logHttpError(r, w, err, http.StatusBadRequest, c.logger)
		return
	}

	id, err := c.storage.RecordIssuance(ssh.UserCert, user, pk)
	if err != nil {
		logHttpError(r, w, err, http.StatusInternalServerError, c.logger)
		return
	}

	certificate, err := c.sign(user, []string{user}, id, ssh.UserCert, pk)
	if err != nil {
		logHttpError(r, w, err, http.StatusInternalServerError, c.logger)
		return
	}

	certString, err := encodeCert(certificate)
	if err != nil {
		logHttpError(r, w, err, http.StatusInternalServerError, c.logger)
		return
	}

	_, _ = w.Write([]byte(certString))

	encodedPublicKey := base64.StdEncoding.EncodeToString(pk.Marshal())
	c.logger.WithFields(logrus.Fields{
		"Type":       pk.Type(),
		"Public Key": encodedPublicKey,
		"user":       user,
	}).Println("call EnrollUser")
}

func getDurationForCertType(cfg *config.Config, certType uint32) (time.Duration, error) {
	var duration time.Duration
	var err error

	switch certType {
	case ssh.HostCert:
		duration, err = time.ParseDuration(cfg.HostCertDuration)
	case ssh.UserCert:
		duration, err = time.ParseDuration(cfg.UserCertDuration)
	default:
		err = fmt.Errorf("unknown cert type %d", certType)
	}

	return duration, err
}

func getPermissionsForCertType(cfg *config.SSH, certType uint32) (perms ssh.Permissions) {
	switch certType {
	case ssh.UserCert:
		if cfg != nil && len(cfg.UserCertExtensions) > 0 {
			perms.Extensions = make(map[string]string, len(cfg.UserCertExtensions))
			for _, ext := range cfg.UserCertExtensions {
				perms.Extensions[ext] = ""
			}
		}
	}
	return
}
