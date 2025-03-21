package objectcount

import (
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	objectcountapi "k8s.io/kubernetes/plugin/pkg/admission/objectcount/apis/objectcount"
)

var (
	scheme = runtime.NewScheme()
	codecs = serializer.NewCodecFactory(scheme)
)

func init() {
	utilruntime.Must(objectcountapi.AddToScheme(scheme))
}

// LoadConfiguration loads the provided configuration.
func LoadConfiguration(config io.Reader) (*objectcountapi.Configuration, error) {
	if config == nil {
		return nil, nil
	}

	data, err := io.ReadAll(config)
	if err != nil {
		return nil, err
	}
	decoder := codecs.UniversalDecoder()
	decodedObj, err := runtime.Decode(decoder, data)
	if err != nil {
		return nil, err
	}
	objectCountConfiguration, ok := decodedObj.(*objectcountapi.Configuration)
	if !ok {
		return nil, fmt.Errorf("unexpected type: %T", decodedObj)
	}
	SetDefaults(objectCountConfiguration)
	return objectCountConfiguration, nil
}

func SetDefaults(obj *objectcountapi.Configuration) {
	if obj.RefreshIntervalSeconds == nil {
		interval := int64(defaultRefreshInterval)
		obj.RefreshIntervalSeconds = &interval
	}
}
