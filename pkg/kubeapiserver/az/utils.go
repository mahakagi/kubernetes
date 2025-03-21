/*
Copyright 2017 The Kubernetes Authors.

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

package az

import (
	"strconv"
	"time"
)

const (
	// Label applied on the lease object. The value must be availability-zone-id
	// The label is applied by the az resiliency poller https://gitlab.aws.dev/eks-dataplane/eks-dataplane-az-poller
	// Refer https://quip-amazon.com/bgPkAo16pbXT/Dataplane-Pollers-for-AZ-Resiliency for details.
	ZoneEvacuationLabelKey = "eks.amazonaws.com/evacuate-zone"
	// Label applied on the lease object. The value must be an Unix epoch seconds. Unix epoch always represents UTC time.
	// The label is applied by the az resiliency poller https://gitlab.aws.dev/eks-dataplane/eks-dataplane-az-poller
	// Refer https://quip-amazon.com/bgPkAo16pbXT/Dataplane-Pollers-for-AZ-Resiliency for details.
	// Empty value behaves the same as an expired time. If time specified in the value is expired, zone evacuation stops.
	ZoneEvacuationExpiryLabelKey = "eks.amazonaws.com/evacuate-zone-expiry"
	// The current availability-zone-id the apiserver is operating in. This is an environment variable set in the
	// apiserver static pod manifest in https://gitlab.aws.dev/eks-dataplane/eks-kcp-ami-config
	// EKS assumes that all lease updates from controllers occur within the same host.
	CurrentZoneEnvironmentKey = "APISERVER_AVAILABILITY_ZONE"
)

func IsEpochCurrent(zoneEvacuationExpiry string) bool {
	if zoneEvacuationExpiry == "" {
		return false
	}

	if expiryEpoch, err := strconv.ParseInt(zoneEvacuationExpiry, 10, 64); err == nil && expiryEpoch > time.Now().UTC().Unix() {
		return true
	}
	return false
}
