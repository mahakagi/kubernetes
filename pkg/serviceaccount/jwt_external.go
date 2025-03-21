/*
Copyright 2019 The Kubernetes Authors.

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

package serviceaccount

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	jose "gopkg.in/go-jose/go-jose.v2"
	"gopkg.in/go-jose/go-jose.v2/jwt"

	"k8s.io/apiserver/pkg/authentication/authenticator"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/keyutil"
	"k8s.io/klog/v2"
	externalsigner "k8s.io/kubernetes/pkg/serviceaccount/externalsigner/v1alpha1"
)

// ExternalJWTTokenGenerator returns a TokenGenerator that generates signed JWT Tokens using a remote signing service
func ExternalJWTTokenGenerator(iss string, socketPath string) (TokenGenerator, error) {
	// TODO: @micahhausler conditionally add unix:// prefix
	conn, err := grpc.Dial(fmt.Sprintf("unix://%s", socketPath), grpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	generator := &ExternalTokenGenerator{
		Iss:    iss,
		Client: externalsigner.NewKeyServiceClient(conn),
	}
	return generator, nil
}

var _ TokenGenerator = &ExternalTokenGenerator{}

type ExternalTokenGenerator struct {
	Iss    string
	Client externalsigner.KeyServiceClient
}

func (g *ExternalTokenGenerator) GenerateToken(ctx context.Context, claims *jwt.Claims, privateClaims interface{}) (string, error) {
	signer := NewRemoteOpaqueSigner(g.Client)
	generator, err := JWTTokenGenerator(g.Iss, signer)
	if err != nil {
		return "", err
	}
	return generator.GenerateToken(ctx, claims, privateClaims)
}

// NewRemoteOpaqueSigner returns an jose.OpaqueSigner that communicates over the client
func NewRemoteOpaqueSigner(client externalsigner.KeyServiceClient) *RemoteOpaqueSigner {
	return &RemoteOpaqueSigner{Client: client}
}

type RemoteOpaqueSigner struct {
	Client externalsigner.KeyServiceClient
}

// check that OpaqueSigner conforms to the interface
var _ jose.OpaqueSigner = &RemoteOpaqueSigner{}

func (s *RemoteOpaqueSigner) Public() *jose.JSONWebKey {
	resp, err := s.Client.ListPublicKeys(context.Background(), &externalsigner.ListPublicKeysRequest{})
	if err != nil {
		klog.Errorf("Error getting public keys: %v", err)
		return nil
	}
	var currentPublicKey *externalsigner.PublicKey
	for _, key := range resp.PublicKeys {
		if resp.ActiveKeyId == key.KeyId {
			currentPublicKey = key
			break
		}
	}
	if currentPublicKey == nil {
		klog.Errorf("Current key_id %s not found in list", resp.ActiveKeyId)
		return nil
	}
	response := &jose.JSONWebKey{
		KeyID:     currentPublicKey.KeyId,
		Algorithm: currentPublicKey.Algorithm,
		Use:       "sig",
	}
	keys, err := keyutil.ParsePublicKeysPEM(currentPublicKey.PublicKey)
	if err != nil {
		klog.Errorf("Error getting public key: %v", err)
		return nil
	}
	if len(keys) == 0 {
		klog.Error("No public key returned")
		return nil
	}
	response.Key = keys[0]
	response.Certificates, err = certutil.ParseCertsPEM(currentPublicKey.Certificates)
	if err != nil && err != certutil.ErrNoCerts {
		klog.Errorf("Error parsing x509 certificate: %v", err)
		return nil
	}
	return response
}

func (s *RemoteOpaqueSigner) Algs() []jose.SignatureAlgorithm {
	resp, err := s.Client.ListPublicKeys(context.Background(), &externalsigner.ListPublicKeysRequest{})
	if err != nil {
		klog.Errorf("Error getting public keys: %v", err)
		return nil
	}
	algos := map[string]bool{}
	for _, key := range resp.PublicKeys {
		algos[key.Algorithm] = true
	}
	response := []jose.SignatureAlgorithm{}
	for alg := range algos {
		response = append(response, jose.SignatureAlgorithm(alg))
	}
	return response
}

func (s *RemoteOpaqueSigner) SignPayload(payload []byte, alg jose.SignatureAlgorithm) ([]byte, error) {
	resp, err := s.Client.SignPayload(context.Background(), &externalsigner.SignPayloadRequest{
		Payload:   payload,
		Algorithm: string(alg),
	})
	if err != nil {
		return nil, err
	}
	return resp.Content, nil
}

type ExternalTokenAuthenticator[PrivateClaims any] struct {
	Client       externalsigner.KeyServiceClient
	Issuers      []string
	IssuersMap   map[string]bool
	Validator    Validator[PrivateClaims]
	ImplicitAuds authenticator.Audiences
}

func (a *ExternalTokenAuthenticator[PrivateClaims]) AuthenticateToken(ctx context.Context, tokenData string) (*authenticator.Response, bool, error) {
	if !a.hasCorrectIssuer(tokenData) {
		return nil, false, nil
	}

	keyResp, err := a.Client.ListPublicKeys(ctx, &externalsigner.ListPublicKeysRequest{})
	if err != nil {
		return nil, false, err
	}
	var keyData []byte
	for _, pubKey := range keyResp.PublicKeys {
		keyData = append(keyData, pubKey.PublicKey...)
		keyData = append(keyData, '\n')
	}
	keys, err := keyutil.ParsePublicKeysPEM(keyData)
	if err != nil {
		return nil, false, err
	}
	// the keys are parsed from the content of the public keys, so the number of parsed keys and
	// the order of parsed keys in the slice should match the input public keys
	if len(keyResp.PublicKeys) != len(keys) {
		return nil, false, fmt.Errorf("number of keys parsed does not match number of input public keys")
	}
	// directly use key id of the input public keys instead of using SHA256 like https://github.com/kubernetes/kubernetes/pull/125177
	// did in StaticPublicKeysGetter, because the keys are from external signer instead of kubernetes itself. So we don't need to
	// follow the pattern that is understandable to Kubernetes.
	var publicKeys []PublicKey
	for i, key := range keys {
		publicKeys = append(publicKeys, PublicKey{KeyID: keyResp.PublicKeys[i].KeyId, PublicKey: key})
	}
	publicKeysGetter, err := ExternalStaticPublicKeysGetter(publicKeys)

	if err != nil {
		return nil, false, err
	}
	return JWTTokenAuthenticator(
		a.Issuers,
		publicKeysGetter,
		a.ImplicitAuds,
		a.Validator,
	).AuthenticateToken(
		ctx, tokenData,
	)
}

// ExternalJWTTokenAuthenticator authenticates JWT tokens signed externally
func ExternalJWTTokenAuthenticator[PrivateClaims any](socketPath string, issuers []string, implicitAuds authenticator.Audiences, validator Validator[PrivateClaims]) (authenticator.Token, error) {
	conn, err := grpc.Dial(fmt.Sprintf("unix://%s", socketPath), grpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	issuersMap := make(map[string]bool)
	for _, issuer := range issuers {
		issuersMap[issuer] = true
	}
	return &ExternalTokenAuthenticator[PrivateClaims]{
		Client:       externalsigner.NewKeyServiceClient(conn),
		Issuers:      issuers,
		IssuersMap:   issuersMap,
		Validator:    validator,
		ImplicitAuds: implicitAuds,
	}, nil
}

// hasCorrectIssuer returns true if tokenData is a valid JWT in compact
// serialization format and the "iss" claim matches the iss field of this token
// authenticator, and otherwise returns false.
//
// Note: go-jose currently does not allow access to unverified JWS payloads.
// See https://github.com/square/go-jose/issues/169
func (a *ExternalTokenAuthenticator[PrivateClaims]) hasCorrectIssuer(tokenData string) bool {
	parts := strings.Split(tokenData, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	claims := struct {
		// WARNING: this JWT is not verified. Do not trust these claims.
		Issuer string `json:"iss"`
	}{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return false
	}
	return a.IssuersMap[claims.Issuer]
}

type externalStaticPublicKeysGetter struct {
	allPublicKeys  []PublicKey
	publicKeysByID map[string][]PublicKey
}

// ExternalStaticPublicKeysGetter constructs an implementation of PublicKeysGetter
// which returns all public keys when key id is unspecified, and returns
// the public keys matching the keyIDFromPublicKey-derived key id when
// a key id is specified.
//
// ExternalStaticPublicKeysGetter has the same implementation as StaticPublicKeysGetter
// except it directly uses the key id of external signer's public keys as the key id
// in the keys map, instead of deriving it from the public key content.
func ExternalStaticPublicKeysGetter(keys []PublicKey) (PublicKeysGetter, error) {
	publicKeysByID := map[string][]PublicKey{}
	for _, key := range keys {
		publicKeysByID[key.KeyID] = append(publicKeysByID[key.KeyID], key)
	}
	return &staticPublicKeysGetter{
		allPublicKeys:  keys,
		publicKeysByID: publicKeysByID,
	}, nil
}

func (e externalStaticPublicKeysGetter) AddListener(listener Listener) {
	// no-op, static key content never changes
}

func (e externalStaticPublicKeysGetter) GetCacheAgeMaxSeconds() int {
	// hard-coded to match cache max-age set in OIDC discovery
	return 3600
}

func (e externalStaticPublicKeysGetter) GetPublicKeys(keyID string) []PublicKey {
	if len(keyID) == 0 {
		return e.allPublicKeys
	}
	return e.publicKeysByID[keyID]
}
