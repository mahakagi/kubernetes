/*
Copyright 2023 The Kubernetes Authors.

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

package features

import (
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/component-base/featuregate"
)

const (
	Alpha          featuregate.Feature = "TestAlphaFeature"
	Beta           featuregate.Feature = "TestBetaFeature"
	BetaDefaultOff featuregate.Feature = "TestBetaDefaultOffFeature"
	GA             featuregate.Feature = "TestGAFeature"
)

func init() {
	runtime.Must(utilfeature.DefaultMutableFeatureGate.AddVersioned(testFeatureGates))
}

var testFeatureGates = map[featuregate.Feature]featuregate.VersionedSpecs{
	Alpha: {
		{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Alpha},
	},
	Beta: {
		{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.28"), Default: true, PreRelease: featuregate.Beta},
	},
	BetaDefaultOff: {
		{Version: version.MustParse("1.28"), Default: false, PreRelease: featuregate.Beta},
	},
	GA: {
		{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.28"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},
}
