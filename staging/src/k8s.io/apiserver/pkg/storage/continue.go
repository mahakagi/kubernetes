/*
Copyright 2022 The Kubernetes Authors.

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

package storage

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
)

var (
	ErrInvalidStartRV             = errors.New("continue key is not valid: incorrect encoded start resourceVersion (version meta.k8s.io/v1)")
	ErrEmptyStartKey              = errors.New("continue key is not valid: encoded start key empty (version meta.k8s.io/v1)")
	ErrGenericInvalidKey          = errors.New("continue key is not valid")
	ErrUnrecognizedEncodedVersion = errors.New("continue key is not valid: server does not recognize this encoded version")
)

// continueToken is a simple structured object for encoding the state of a continue token.
// TODO: if we change the version of the encoded from, we can't start encoding the new version
// until all other servers are upgraded (i.e. we need to support rolling schema)
// This is a public API struct and cannot change.
type continueToken struct {
	APIVersion      string `json:"v"`
	ResourceVersion int64  `json:"rv"`
	StartKey        string `json:"start"`
	RemainingCount  int64  `json:"rc,omitempty"`
}

// DecodeContinue transforms an encoded predicate from into a versioned struct.
// TODO: return a typed error that instructs clients that they must relist
func DecodeContinue(continueValue, keyPrefix string) (fromKey string, rv int64, rc int64, err error) {
	data, err := base64.RawURLEncoding.DecodeString(continueValue)
	if err != nil {
		return "", 0, 0, fmt.Errorf("%w: %v", ErrGenericInvalidKey, err)
	}
	var c continueToken
	if err := json.Unmarshal(data, &c); err != nil {
		return "", 0, 0, fmt.Errorf("%w: %v", ErrGenericInvalidKey, err)
	}
	switch c.APIVersion {
	case "meta.k8s.io/v1":
		if c.ResourceVersion == 0 {
			return "", 0, 0, ErrInvalidStartRV
		}
		if len(c.StartKey) == 0 {
			return "", 0, 0, ErrEmptyStartKey
		}
		// defend against path traversal attacks by clients - path.Clean will ensure that startKey cannot
		// be at a higher level of the hierarchy, and so when we append the key prefix we will end up with
		// continue start key that is fully qualified and cannot range over anything less specific than
		// keyPrefix.
		key := c.StartKey
		if !strings.HasPrefix(key, "/") {
			key = "/" + key
		}
		cleaned := path.Clean(key)
		if cleaned != key {
			return "", 0, 0, fmt.Errorf("%w: %v", ErrGenericInvalidKey, c.StartKey)
		}
		return keyPrefix + cleaned[1:], c.ResourceVersion, c.RemainingCount, nil
	default:
		return "", 0, 0, fmt.Errorf("%w %v", ErrUnrecognizedEncodedVersion, c.APIVersion)
	}
}

// EncodeContinue returns a string representing the encoded continuation of the current query.
func EncodeContinue(key, keyPrefix string, resourceVersion int64, remainingCount int64) (string, error) {
	nextKey := strings.TrimPrefix(key, keyPrefix)
	if nextKey == key {
		return "", fmt.Errorf("unable to encode next field: the key and key prefix do not match")
	}
	out, err := json.Marshal(&continueToken{APIVersion: "meta.k8s.io/v1", ResourceVersion: resourceVersion, StartKey: nextKey, RemainingCount: remainingCount})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// PrepareContinueToken prepares optional
// parameters for retrieving additional results for a paginated request.
//
// This function sets up parameters that a client can use to fetch the remaining results
// from the server if they are available.
func PrepareContinueToken(keyLastItem, keyPrefix string, resourceVersion int64, itemsCount int64, hasMoreItems bool, opts ListOptions, enableFastCount bool) (string, *int64, error) {
	var remainingItemCount *int64
	var continueValue string
	var err error

	if hasMoreItems {
		// when predicate is not empty, we need to pass 0 as ric to EncodeContinue
		var newRIC int64
		// getResp.Count counts in objects that do not match the pred.
		// Instead of returning inaccurate count for non-empty selectors, we return nil.
		// Only set remainingItemCount if the predicate is empty.
		if opts.Predicate.Empty() {
			newRIC = int64(itemsCount - opts.Predicate.Limit)
			remainingItemCount = &newRIC
		}
		// Instruct the client to begin querying from immediately after the last key.
		// Default value for remainingItemCount if fast count is disabled
		ric := int64(0)
		if enableFastCount {
			// Use newRIC only when fast count is enabled
			ric = newRIC
		}
		continueValue, err = EncodeContinue(keyLastItem+"\x00", keyPrefix, resourceVersion, ric)
		if err != nil {
			return "", remainingItemCount, err
		}
	}
	return continueValue, remainingItemCount, err
}
