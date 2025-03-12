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

package validation

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimachineryvalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/environment"
	dracel "k8s.io/dynamic-resource-allocation/cel"
	"k8s.io/dynamic-resource-allocation/structured"
	corevalidation "k8s.io/kubernetes/pkg/apis/core/validation"
	"k8s.io/kubernetes/pkg/apis/resource"
)

var (
	// validateResourceDriverName reuses the validation of a CSI driver because
	// the allowed values are exactly the same.
	validateDriverName      = corevalidation.ValidateCSIDriverName
	validateDeviceName      = corevalidation.ValidateDNS1123Label
	validateDeviceClassName = corevalidation.ValidateDNS1123Subdomain
	validateRequestName     = corevalidation.ValidateDNS1123Label
	validateCounterName     = corevalidation.ValidateDNS1123Label

	// this is the max length limit for domain/ID
	attributeAndCapacityMaxKeyLength = resource.DeviceMaxDomainLength + 1 + resource.DeviceMaxIDLength
)

func validatePoolName(name string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if name == "" {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	} else {
		if len(name) > resource.PoolNameMaxLength {
			allErrs = append(allErrs, field.TooLong(fldPath, "" /*unused*/, resource.PoolNameMaxLength))
		}
		parts := strings.Split(name, "/")
		for _, part := range parts {
			allErrs = append(allErrs, corevalidation.ValidateDNS1123Subdomain(part, fldPath)...)
		}
	}
	return allErrs
}

// ValidateResourceClaim validates a ResourceClaim.
func ValidateResourceClaim(resourceClaim *resource.ResourceClaim) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMeta(&resourceClaim.ObjectMeta, true, corevalidation.ValidateResourceClaimName, field.NewPath("metadata"))
	allErrs = append(allErrs, validateResourceClaimSpec(&resourceClaim.Spec, field.NewPath("spec"), false)...)
	return allErrs
}

// ValidateResourceClaimUpdate tests if an update to ResourceClaim is valid.
func ValidateResourceClaimUpdate(resourceClaim, oldClaim *resource.ResourceClaim) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMetaUpdate(&resourceClaim.ObjectMeta, &oldClaim.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, apimachineryvalidation.ValidateImmutableField(resourceClaim.Spec, oldClaim.Spec, field.NewPath("spec"))...)
	// Because the spec is immutable, all CEL expressions in it must have been stored.
	// If the user tries an update, this is not true and checking is less strict, but
	// as there are errors, it doesn't matter.
	allErrs = append(allErrs, validateResourceClaimSpec(&resourceClaim.Spec, field.NewPath("spec"), true)...)
	return allErrs
}

// ValidateResourceClaimStatusUpdate tests if an update to the status of a ResourceClaim is valid.
func ValidateResourceClaimStatusUpdate(resourceClaim, oldClaim *resource.ResourceClaim) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMetaUpdate(&resourceClaim.ObjectMeta, &oldClaim.ObjectMeta, field.NewPath("metadata"))
	requestNames := gatherRequestNames(&resourceClaim.Spec.Devices)
	allErrs = append(allErrs, validateResourceClaimStatusUpdate(&resourceClaim.Status, &oldClaim.Status, resourceClaim.DeletionTimestamp != nil, requestNames, field.NewPath("status"))...)
	return allErrs
}

func validateResourceClaimSpec(spec *resource.ResourceClaimSpec, fldPath *field.Path, stored bool) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, validateDeviceClaim(&spec.Devices, fldPath.Child("devices"), stored)...)
	return allErrs
}

func validateDeviceClaim(deviceClaim *resource.DeviceClaim, fldPath *field.Path, stored bool) field.ErrorList {
	allErrs := field.ErrorList{}
	requestNames := gatherRequestNames(deviceClaim)
	allErrs = append(allErrs, validateSet(deviceClaim.Requests, resource.DeviceRequestsMaxSize,
		func(request resource.DeviceRequest, fldPath *field.Path) field.ErrorList {
			return validateDeviceRequest(request, fldPath, stored)
		},
		func(request resource.DeviceRequest) (string, string) {
			return request.Name, "name"
		},
		fldPath.Child("requests"))...)
	allErrs = append(allErrs, validateSlice(deviceClaim.Constraints, resource.DeviceConstraintsMaxSize,
		func(constraint resource.DeviceConstraint, fldPath *field.Path) field.ErrorList {
			return validateDeviceConstraint(constraint, fldPath, requestNames)
		}, fldPath.Child("constraints"))...)
	allErrs = append(allErrs, validateSlice(deviceClaim.Config, resource.DeviceConfigMaxSize,
		func(config resource.DeviceClaimConfiguration, fldPath *field.Path) field.ErrorList {
			return validateDeviceClaimConfiguration(config, fldPath, requestNames, stored)
		}, fldPath.Child("config"))...)
	return allErrs
}

type requestNames map[string]sets.Set[string]

func (r requestNames) Has(s string) bool {
	segments := strings.Split(s, "/")
	// If there are more than one / in the string, we
	// know there can't be any match.
	if len(segments) > 2 {
		return false
	}
	// If the first segment doesn't have a match, we
	// don't need to check the other one.
	subRequestNames, found := r[segments[0]]
	if !found {
		return false
	}
	if len(segments) == 1 {
		return true
	}
	// If the first segment matched and we have another one,
	// check for a match for that too.
	return subRequestNames.Has(segments[1])
}

func gatherRequestNames(deviceClaim *resource.DeviceClaim) requestNames {
	requestNames := make(requestNames)
	for _, request := range deviceClaim.Requests {
		if len(request.FirstAvailable) == 0 {
			requestNames[request.Name] = nil
			continue
		}
		subRequestNames := sets.New[string]()
		for _, subRequest := range request.FirstAvailable {
			subRequestNames.Insert(subRequest.Name)
		}
		requestNames[request.Name] = subRequestNames
	}
	return requestNames
}

func gatherAllocatedDevices(allocationResult *resource.DeviceAllocationResult) sets.Set[structured.DeviceID] {
	allocatedDevices := sets.New[structured.DeviceID]()
	for _, result := range allocationResult.Results {
		deviceID := structured.MakeDeviceID(result.Driver, result.Pool, result.Device)
		allocatedDevices.Insert(deviceID)
	}
	return allocatedDevices
}

func validateDeviceRequest(request resource.DeviceRequest, fldPath *field.Path, stored bool) field.ErrorList {
	allErrs := validateRequestName(request.Name, fldPath.Child("name"))

	if request.DeviceClassName == "" && len(request.FirstAvailable) == 0 {
		allErrs = append(allErrs, field.Required(fldPath, "exactly one of `deviceClassName` or `firstAvailable` must be specified"))
	} else if request.DeviceClassName != "" && len(request.FirstAvailable) > 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, nil, "exactly one of `deviceClassName` or `firstAvailable` must be specified"))
	} else if request.DeviceClassName != "" {
		allErrs = append(allErrs, validateDeviceClass(request.DeviceClassName, fldPath.Child("deviceClassName"))...)
		allErrs = append(allErrs, validateSelectorSlice(request.Selectors, fldPath.Child("selectors"), stored)...)
		allErrs = append(allErrs, validateDeviceAllocationMode(request.AllocationMode, request.Count, fldPath.Child("allocationMode"), fldPath.Child("count"))...)
	} else if len(request.FirstAvailable) > 0 {
		if request.Selectors != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("selectors"), request.Selectors, "must not be specified when firstAvailable is set"))
		}
		if request.AllocationMode != "" {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("allocationMode"), request.AllocationMode, "must not be specified when firstAvailable is set"))
		}
		if request.Count != 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("count"), request.Count, "must not be specified when firstAvailable is set"))
		}
		if request.AdminAccess != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("adminAccess"), request.AdminAccess, "must not be specified when firstAvailable is set"))
		}
		allErrs = append(allErrs, validateSet(request.FirstAvailable, resource.FirstAvailableDeviceRequestMaxSize,
			func(subRequest resource.DeviceSubRequest, fldPath *field.Path) field.ErrorList {
				return validateDeviceSubRequest(subRequest, fldPath, stored)
			},
			func(subRequest resource.DeviceSubRequest) (string, string) {
				return subRequest.Name, "name"
			},
			fldPath.Child("firstAvailable"))...)
	}
	for i, toleration := range request.Tolerations {
		allErrs = append(allErrs, validateDeviceToleration(toleration, fldPath.Child("tolerations").Index(i))...)
	}

	return allErrs
}

func validateDeviceSubRequest(subRequest resource.DeviceSubRequest, fldPath *field.Path, stored bool) field.ErrorList {
	allErrs := validateRequestName(subRequest.Name, fldPath.Child("name"))
	allErrs = append(allErrs, validateDeviceClass(subRequest.DeviceClassName, fldPath.Child("deviceClassName"))...)
	allErrs = append(allErrs, validateSelectorSlice(subRequest.Selectors, fldPath.Child("selectors"), stored)...)
	allErrs = append(allErrs, validateDeviceAllocationMode(subRequest.AllocationMode, subRequest.Count, fldPath.Child("allocationMode"), fldPath.Child("count"))...)
	for i, toleration := range subRequest.Tolerations {
		allErrs = append(allErrs, validateDeviceToleration(toleration, fldPath.Child("tolerations").Index(i))...)
	}
	return allErrs
}

func validateDeviceAllocationMode(deviceAllocationMode resource.DeviceAllocationMode, count int64, allocModeFldPath, countFldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	switch deviceAllocationMode {
	case resource.DeviceAllocationModeAll:
		if count != 0 {
			allErrs = append(allErrs, field.Invalid(countFldPath, count, fmt.Sprintf("must not be specified when allocationMode is '%s'", deviceAllocationMode)))
		}
	case resource.DeviceAllocationModeExactCount:
		if count <= 0 {
			allErrs = append(allErrs, field.Invalid(countFldPath, count, "must be greater than zero"))
		}
	default:
		allErrs = append(allErrs, field.NotSupported(allocModeFldPath, deviceAllocationMode, []resource.DeviceAllocationMode{resource.DeviceAllocationModeAll, resource.DeviceAllocationModeExactCount}))
	}
	return allErrs
}

func validateDeviceClass(deviceClass string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if deviceClass == "" {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	} else {
		allErrs = append(allErrs, validateDeviceClassName(deviceClass, fldPath)...)
	}
	return allErrs
}

func validateSelectorSlice(selectors []resource.DeviceSelector, fldPath *field.Path, stored bool) field.ErrorList {
	return validateSlice(selectors, resource.DeviceSelectorsMaxSize,
		func(selector resource.DeviceSelector, fldPath *field.Path) field.ErrorList {
			return validateSelector(selector, fldPath, stored)
		},
		fldPath)
}

func validateSelector(selector resource.DeviceSelector, fldPath *field.Path, stored bool) field.ErrorList {
	var allErrs field.ErrorList
	if selector.CEL == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("cel"), ""))
	} else {
		allErrs = append(allErrs, validateCELSelector(*selector.CEL, fldPath.Child("cel"), stored)...)
	}
	return allErrs
}

func validateCELSelector(celSelector resource.CELDeviceSelector, fldPath *field.Path, stored bool) field.ErrorList {
	var allErrs field.ErrorList
	envType := environment.NewExpressions
	if stored {
		envType = environment.StoredExpressions
	}
	if len(celSelector.Expression) > resource.CELSelectorExpressionMaxLength {
		allErrs = append(allErrs, field.TooLong(fldPath.Child("expression"), "" /*unused*/, resource.CELSelectorExpressionMaxLength))
		// Don't bother compiling too long expressions.
		return allErrs
	}

	result := dracel.GetCompiler().CompileCELExpression(celSelector.Expression, dracel.Options{EnvType: &envType})
	if result.Error != nil {
		allErrs = append(allErrs, convertCELErrorToValidationError(fldPath.Child("expression"), celSelector.Expression, result.Error))
	} else if result.MaxCost > resource.CELSelectorExpressionMaxCost {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("expression"), "too complex, exceeds cost limit"))
	}

	return allErrs
}

func convertCELErrorToValidationError(fldPath *field.Path, expression string, err error) *field.Error {
	var celErr *cel.Error
	if errors.As(err, &celErr) {
		switch celErr.Type {
		case cel.ErrorTypeRequired:
			return field.Required(fldPath, celErr.Detail)
		case cel.ErrorTypeInvalid:
			return field.Invalid(fldPath, expression, celErr.Detail)
		case cel.ErrorTypeInternal:
			return field.InternalError(fldPath, celErr)
		}
	}
	return field.InternalError(fldPath, fmt.Errorf("unsupported error type: %w", err))
}

func validateDeviceConstraint(constraint resource.DeviceConstraint, fldPath *field.Path, requestNames requestNames) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateSet(constraint.Requests, resource.DeviceRequestsMaxSize,
		func(name string, fldPath *field.Path) field.ErrorList {
			return validateRequestNameRef(name, fldPath, requestNames)
		},
		stringKey, fldPath.Child("requests"))...)
	if constraint.MatchAttribute == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("matchAttribute"), ""))
	} else {
		allErrs = append(allErrs, validateFullyQualifiedName(*constraint.MatchAttribute, fldPath.Child("matchAttribute"))...)
	}
	return allErrs
}

func validateDeviceClaimConfiguration(config resource.DeviceClaimConfiguration, fldPath *field.Path, requestNames requestNames, stored bool) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateSet(config.Requests, resource.DeviceRequestsMaxSize,
		func(name string, fldPath *field.Path) field.ErrorList {
			return validateRequestNameRef(name, fldPath, requestNames)
		}, stringKey, fldPath.Child("requests"))...)
	allErrs = append(allErrs, validateDeviceConfiguration(config.DeviceConfiguration, fldPath, stored)...)
	return allErrs
}

func validateRequestNameRef(name string, fldPath *field.Path, requestNames requestNames) field.ErrorList {
	var allErrs field.ErrorList
	segments := strings.Split(name, "/")
	if len(segments) > 2 {
		allErrs = append(allErrs, field.Invalid(fldPath, name, "must be the name of a request in the claim or the name of a request and a subrequest separated by '/'"))
		return allErrs
	}

	for i := range segments {
		allErrs = append(allErrs, validateRequestName(segments[i], fldPath)...)
	}

	if !requestNames.Has(name) {
		allErrs = append(allErrs, field.Invalid(fldPath, name, "must be the name of a request in the claim or the name of a request and a subrequest separated by '/'"))
	}
	return allErrs
}

func validateDeviceConfiguration(config resource.DeviceConfiguration, fldPath *field.Path, stored bool) field.ErrorList {
	var allErrs field.ErrorList
	if config.Opaque == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("opaque"), ""))
	} else {
		allErrs = append(allErrs, validateOpaqueConfiguration(*config.Opaque, fldPath.Child("opaque"), stored)...)
	}
	return allErrs
}

func validateOpaqueConfiguration(config resource.OpaqueDeviceConfiguration, fldPath *field.Path, stored bool) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateDriverName(config.Driver, fldPath.Child("driver"))...)
	allErrs = append(allErrs, validateRawExtension(config.Parameters, fldPath.Child("parameters"), stored, resource.OpaqueParametersMaxLength)...)
	return allErrs
}

func validateResourceClaimStatusUpdate(status, oldStatus *resource.ResourceClaimStatus, claimDeleted bool, requestNames requestNames, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateSet(status.ReservedFor, resource.ResourceClaimReservedForMaxSize,
		validateResourceClaimUserReference,
		func(consumer resource.ResourceClaimConsumerReference) (types.UID, string) { return consumer.UID, "uid" },
		fldPath.Child("reservedFor"))...)

	var allocatedDevices sets.Set[structured.DeviceID]
	if status.Allocation != nil {
		allocatedDevices = gatherAllocatedDevices(&status.Allocation.Devices)
	}
	allErrs = append(allErrs, validateSet(status.Devices, -1,
		func(device resource.AllocatedDeviceStatus, fldPath *field.Path) field.ErrorList {
			return validateDeviceStatus(device, fldPath, allocatedDevices)
		},
		func(device resource.AllocatedDeviceStatus) (structured.DeviceID, string) {
			return structured.MakeDeviceID(device.Driver, device.Pool, device.Device), "deviceID"
		},
		fldPath.Child("devices"))...)

	// Now check for invariants that must be valid for a ResourceClaim.
	if len(status.ReservedFor) > 0 {
		if status.Allocation == nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("reservedFor"), "may not be specified when `allocated` is not set"))
		} else {
			// Items may be removed from ReservedFor while the claim is meant to be deallocated,
			// but not added.
			if claimDeleted {
				oldSet := sets.New(oldStatus.ReservedFor...)
				newSet := sets.New(status.ReservedFor...)
				newItems := newSet.Difference(oldSet)
				if len(newItems) > 0 {
					allErrs = append(allErrs, field.Forbidden(fldPath.Child("reservedFor"), "new entries may not be added while `deallocationRequested` or `deletionTimestamp` are set"))
				}
			}
		}
	}

	// Updates to a populated status.Allocation are not allowed.
	// Unmodified fields don't need to be validated again and,
	// in this particular case, must not be validated again because
	// validation for new results is tighter than it was before.
	if oldStatus.Allocation != nil && status.Allocation != nil {
		allErrs = append(allErrs, apimachineryvalidation.ValidateImmutableField(status.Allocation, oldStatus.Allocation, fldPath.Child("allocation"))...)
	} else if status.Allocation != nil {
		allErrs = append(allErrs, validateAllocationResult(status.Allocation, fldPath.Child("allocation"), requestNames, false)...)
	}

	return allErrs
}

func validateResourceClaimUserReference(ref resource.ResourceClaimConsumerReference, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if ref.Resource == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("resource"), ""))
	}
	if ref.Name == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	}
	if ref.UID == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("uid"), ""))
	}
	return allErrs
}

// validateAllocationResult enforces constraints for *new* results, which in at
// least one case (admin access) are more strict than before. Therefore it
// may not be called to re-validate results which were stored earlier.
func validateAllocationResult(allocation *resource.AllocationResult, fldPath *field.Path, requestNames requestNames, stored bool) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateDeviceAllocationResult(allocation.Devices, fldPath.Child("devices"), requestNames, stored)...)
	if allocation.NodeSelector != nil {
		allErrs = append(allErrs, corevalidation.ValidateNodeSelector(allocation.NodeSelector, false, fldPath.Child("nodeSelector"))...)
	}
	return allErrs
}

func validateDeviceAllocationResult(allocation resource.DeviceAllocationResult, fldPath *field.Path, requestNames requestNames, stored bool) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateSlice(allocation.Results, resource.AllocationResultsMaxSize,
		func(result resource.DeviceRequestAllocationResult, fldPath *field.Path) field.ErrorList {
			return validateDeviceRequestAllocationResult(result, fldPath, requestNames)
		}, fldPath.Child("results"))...)
	allErrs = append(allErrs, validateSlice(allocation.Config, 2*resource.DeviceConfigMaxSize, /* class + claim */
		func(config resource.DeviceAllocationConfiguration, fldPath *field.Path) field.ErrorList {
			return validateDeviceAllocationConfiguration(config, fldPath, requestNames, stored)
		}, fldPath.Child("config"))...)

	return allErrs
}

func validateDeviceRequestAllocationResult(result resource.DeviceRequestAllocationResult, fldPath *field.Path, requestNames requestNames) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateRequestNameRef(result.Request, fldPath.Child("request"), requestNames)...)
	allErrs = append(allErrs, validateDriverName(result.Driver, fldPath.Child("driver"))...)
	allErrs = append(allErrs, validatePoolName(result.Pool, fldPath.Child("pool"))...)
	allErrs = append(allErrs, validateDeviceName(result.Device, fldPath.Child("device"))...)
	return allErrs
}

func validateDeviceAllocationConfiguration(config resource.DeviceAllocationConfiguration, fldPath *field.Path, requestNames requestNames, stored bool) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateAllocationConfigSource(config.Source, fldPath.Child("source"))...)
	allErrs = append(allErrs, validateSet(config.Requests, resource.DeviceRequestsMaxSize,
		func(name string, fldPath *field.Path) field.ErrorList {
			return validateRequestNameRef(name, fldPath, requestNames)
		}, stringKey, fldPath.Child("requests"))...)
	allErrs = append(allErrs, validateDeviceConfiguration(config.DeviceConfiguration, fldPath, stored)...)
	return allErrs
}

func validateAllocationConfigSource(source resource.AllocationConfigSource, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	switch source {
	case "":
		allErrs = append(allErrs, field.Required(fldPath, ""))
	case resource.AllocationConfigSourceClaim, resource.AllocationConfigSourceClass:
	default:
		allErrs = append(allErrs, field.NotSupported(fldPath, source, []resource.AllocationConfigSource{resource.AllocationConfigSourceClaim, resource.AllocationConfigSourceClass}))
	}
	return allErrs
}

// ValidateDeviceClass validates a DeviceClass.
func ValidateDeviceClass(class *resource.DeviceClass) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMeta(&class.ObjectMeta, false, corevalidation.ValidateClassName, field.NewPath("metadata"))
	allErrs = append(allErrs, validateDeviceClassSpec(&class.Spec, nil, field.NewPath("spec"))...)
	return allErrs
}

// ValidateDeviceClassUpdate tests if an update to DeviceClass is valid.
func ValidateDeviceClassUpdate(class, oldClass *resource.DeviceClass) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMetaUpdate(&class.ObjectMeta, &oldClass.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, validateDeviceClassSpec(&class.Spec, &oldClass.Spec, field.NewPath("spec"))...)
	return allErrs
}

func validateDeviceClassSpec(spec, oldSpec *resource.DeviceClassSpec, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	// If the selectors are exactly as before, we treat the CEL expressions as "stored".
	// Any change, including merely reordering selectors, triggers validation as new
	// expressions.
	stored := false
	if oldSpec != nil {
		stored = apiequality.Semantic.DeepEqual(spec.Selectors, oldSpec.Selectors)
	}
	allErrs = append(allErrs, validateSlice(spec.Selectors, resource.DeviceSelectorsMaxSize,
		func(selector resource.DeviceSelector, fldPath *field.Path) field.ErrorList {
			return validateSelector(selector, fldPath, stored)
		},
		fldPath.Child("selectors"))...)
	// Same logic as above for configs.
	if oldSpec != nil {
		stored = apiequality.Semantic.DeepEqual(spec.Config, oldSpec.Config)
	}
	allErrs = append(allErrs, validateSlice(spec.Config, resource.DeviceConfigMaxSize,
		func(config resource.DeviceClassConfiguration, fldPath *field.Path) field.ErrorList {
			return validateDeviceClassConfiguration(config, fldPath, stored)
		},
		fldPath.Child("config"))...)
	return allErrs
}

func validateDeviceClassConfiguration(config resource.DeviceClassConfiguration, fldPath *field.Path, stored bool) field.ErrorList {
	return validateDeviceConfiguration(config.DeviceConfiguration, fldPath, stored)
}

// ValidateResourceClaimTemplate validates a ResourceClaimTemplate.
func ValidateResourceClaimTemplate(template *resource.ResourceClaimTemplate) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMeta(&template.ObjectMeta, true, corevalidation.ValidateResourceClaimTemplateName, field.NewPath("metadata"))
	allErrs = append(allErrs, validateResourceClaimTemplateSpec(&template.Spec, field.NewPath("spec"), false)...)
	return allErrs
}

func validateResourceClaimTemplateSpec(spec *resource.ResourceClaimTemplateSpec, fldPath *field.Path, stored bool) field.ErrorList {
	allErrs := corevalidation.ValidateTemplateObjectMeta(&spec.ObjectMeta, fldPath.Child("metadata"))
	allErrs = append(allErrs, validateResourceClaimSpec(&spec.Spec, fldPath.Child("spec"), stored)...)
	return allErrs
}

// ValidateResourceClaimTemplateUpdate tests if an update to template is valid.
func ValidateResourceClaimTemplateUpdate(template, oldTemplate *resource.ResourceClaimTemplate) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMetaUpdate(&template.ObjectMeta, &oldTemplate.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, apimachineryvalidation.ValidateImmutableField(template.Spec, oldTemplate.Spec, field.NewPath("spec"))...)
	// Because the spec is immutable, all CEL expressions in it must have been stored.
	// If the user tries an update, this is not true and checking is less strict, but
	// as there are errors, it doesn't matter.
	allErrs = append(allErrs, validateResourceClaimTemplateSpec(&template.Spec, field.NewPath("spec"), true)...)
	return allErrs
}

func validateNodeName(name string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	for _, msg := range corevalidation.ValidateNodeName(name, false) {
		allErrs = append(allErrs, field.Invalid(fldPath, name, msg))
	}
	return allErrs
}

// ValidateResourceSlice tests if a ResourceSlice object is valid.
func ValidateResourceSlice(slice *resource.ResourceSlice) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMeta(&slice.ObjectMeta, false, apimachineryvalidation.NameIsDNSSubdomain, field.NewPath("metadata"))
	allErrs = append(allErrs, validateResourceSliceSpec(&slice.Spec, nil, field.NewPath("spec"))...)
	return allErrs
}

// ValidateResourceSliceUpdate tests if a ResourceSlice update is valid.
func ValidateResourceSliceUpdate(resourceSlice, oldResourceSlice *resource.ResourceSlice) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMetaUpdate(&resourceSlice.ObjectMeta, &oldResourceSlice.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, validateResourceSliceSpec(&resourceSlice.Spec, &oldResourceSlice.Spec, field.NewPath("spec"))...)
	return allErrs
}

func validateResourceSliceSpec(spec, oldSpec *resource.ResourceSliceSpec, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateDriverName(spec.Driver, fldPath.Child("driver"))...)
	allErrs = append(allErrs, validateResourcePool(spec.Pool, fldPath.Child("pool"))...)
	if oldSpec != nil {
		allErrs = append(allErrs, apimachineryvalidation.ValidateImmutableField(spec.Pool.Name, oldSpec.Pool.Name, fldPath.Child("pool", "name"))...)
		allErrs = append(allErrs, apimachineryvalidation.ValidateImmutableField(spec.Driver, oldSpec.Driver, fldPath.Child("driver"))...)
		allErrs = append(allErrs, apimachineryvalidation.ValidateImmutableField(spec.NodeName, oldSpec.NodeName, fldPath.Child("nodeName"))...)
	}

	setFields := make([]string, 0, 4)
	if spec.NodeName != "" {
		setFields = append(setFields, "`nodeName`")
		allErrs = append(allErrs, validateNodeName(spec.NodeName, fldPath.Child("nodeName"))...)
	}
	if spec.NodeSelector != nil {
		setFields = append(setFields, "`nodeSelector`")
		allErrs = append(allErrs, corevalidation.ValidateNodeSelector(spec.NodeSelector, false, fldPath.Child("nodeSelector"))...)
		if len(spec.NodeSelector.NodeSelectorTerms) != 1 {
			// This additional constraint simplifies merging of different selectors
			// when devices are allocated from different slices.
			allErrs = append(allErrs, field.Invalid(fldPath.Child("nodeSelector", "nodeSelectorTerms"), spec.NodeSelector.NodeSelectorTerms, "must have exactly one node selector term"))
		}
	}
	if spec.AllNodes {
		setFields = append(setFields, "`allNodes`")
	}
	if spec.PerDeviceNodeSelection != nil {
		if *spec.PerDeviceNodeSelection {
			setFields = append(setFields, "`perDeviceNodeSelection`")
		} else {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("perDeviceNodeSelection"), *spec.PerDeviceNodeSelection,
				"must be either unset or set to true"))
		}

	}
	switch len(setFields) {
	case 0:
		allErrs = append(allErrs, field.Required(fldPath, "exactly one of `nodeName`, `nodeSelector`, `allNodes`, `perDeviceNodeSelection` is required"))
	case 1:
	default:
		allErrs = append(allErrs, field.Invalid(fldPath, fmt.Sprintf("{%s}", strings.Join(setFields, ", ")),
			"exactly one of `nodeName`, `nodeSelector`, `allNodes`, `perDeviceNodeSelection` is required, but multiple fields are set"))
	}

	sharedCounterToCounterNames := gatherSharedCounterCounterNames(spec.SharedCounters)
	allErrs = append(allErrs, validateSet(spec.Devices, resource.ResourceSliceMaxDevices,
		func(device resource.Device, fldPath *field.Path) field.ErrorList {
			return validateDevice(device, fldPath, sharedCounterToCounterNames, spec.PerDeviceNodeSelection)
		},
		func(device resource.Device) (string, string) {
			return device.Name, "name"
		}, fldPath.Child("devices"))...)

	allErrs = append(allErrs, validateSet(spec.SharedCounters, resource.ResourceSliceMaxSharedCounters,
		validateCounterSet,
		func(counterSet resource.CounterSet) (string, string) {
			return counterSet.Name, "name"
		}, fldPath.Child("sharedCounters"))...)

	return allErrs
}

func validateCounterSet(counterSet resource.CounterSet, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if counterSet.Name == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	} else {
		allErrs = append(allErrs, validateCounterName(counterSet.Name, fldPath.Child("name"))...)
	}
	if len(counterSet.Counters) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("counters"), ""))
	} else {
		allErrs = append(allErrs, validateMap(counterSet.Counters, resource.ResourceSliceMaxSharedCountersCounters, attributeAndCapacityMaxKeyLength,
			validateCounterName, validateDeviceCounter, fldPath.Child("counters"))...)
	}

	return allErrs
}

func gatherSharedCounterCounterNames(sharedCounters []resource.CounterSet) map[string]sets.Set[string] {
	sharedCounterToCounterMap := make(map[string]sets.Set[string])
	for _, sharedCounter := range sharedCounters {
		counterNames := sets.New[string]()
		for counterName := range sharedCounter.Counters {
			counterNames.Insert(counterName)
		}
		sharedCounterToCounterMap[sharedCounter.Name] = counterNames
	}
	return sharedCounterToCounterMap
}

func validateResourcePool(pool resource.ResourcePool, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validatePoolName(pool.Name, fldPath.Child("name"))...)
	if pool.ResourceSliceCount <= 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("resourceSliceCount"), pool.ResourceSliceCount, "must be greater than zero"))
	}
	if pool.Generation < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("generation"), pool.Generation, "must be greater than or equal to zero"))
	}
	return allErrs
}

func validateDevice(device resource.Device, fldPath *field.Path, sharedCounterToCounterNames map[string]sets.Set[string], perDeviceNodeSelection *bool) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateDeviceName(device.Name, fldPath.Child("name"))...)
	if device.Basic == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("basic"), ""))
	} else {
		allErrs = append(allErrs, validateBasicDevice(*device.Basic, fldPath.Child("basic"), sharedCounterToCounterNames, perDeviceNodeSelection)...)
	}
	return allErrs
}

func validateBasicDevice(device resource.BasicDevice, fldPath *field.Path, sharedCounterToCounterNames map[string]sets.Set[string], perDeviceNodeSelection *bool) field.ErrorList {
	var allErrs field.ErrorList
	// Warn about exceeding the maximum length only once. If any individual
	// field is too large, then so is the combination.
	allErrs = append(allErrs, validateMap(device.Attributes, -1, attributeAndCapacityMaxKeyLength, validateQualifiedName, validateDeviceAttribute, fldPath.Child("attributes"))...)
	allErrs = append(allErrs, validateMap(device.Capacity, -1, attributeAndCapacityMaxKeyLength, validateQualifiedName, validateDeviceCapacity, fldPath.Child("capacity"))...)
	if combinedLen, max := len(device.Attributes)+len(device.Capacity), resource.ResourceSliceMaxAttributesAndCapacitiesPerDevice; combinedLen > max {
		allErrs = append(allErrs, field.Invalid(fldPath, combinedLen, fmt.Sprintf("the total number of attributes and capacities must not exceed %d", max)))
	}
	for i, taint := range device.Taints {
		allErrs = append(allErrs, validateDeviceTaint(taint, fldPath.Child("taints").Index(i))...)
	}

	allErrs = append(allErrs, validateSet(device.ConsumesCounter, resource.ResourceSliceMaxDeviceCounterConsumptions,
		validateDeviceCounterConsumption,
		func(deviceCapacityConsumption resource.DeviceCounterConsumption) (string, string) {
			return deviceCapacityConsumption.SharedCounter, "sharedCounter"
		}, fldPath.Child("consumesCounter"))...)

	for i, deviceCounterConsumption := range device.ConsumesCounter {
		if capacityNames, exists := sharedCounterToCounterNames[deviceCounterConsumption.SharedCounter]; exists {
			for capacityName := range deviceCounterConsumption.Counters {
				if !capacityNames.Has(string(capacityName)) {
					allErrs = append(allErrs, field.Invalid(fldPath.Child("consumesCounter").Index(i).Child("counters"),
						capacityName, "must reference a counter defined in the ResourceSlice sharedCounters"))
				}
			}
		} else {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("consumesCounter").Index(i).Child("sharedCounter"),
				deviceCounterConsumption.SharedCounter, "must reference a counterSet defined in the ResourceSlice sharedCounters"))
		}
	}

	if perDeviceNodeSelection != nil && *perDeviceNodeSelection {
		setFields := make([]string, 0, 3)
		if device.NodeName != nil {
			if len(*device.NodeName) != 0 {
				setFields = append(setFields, "`nodeName`")
				allErrs = append(allErrs, validateNodeName(*device.NodeName, fldPath.Child("nodeName"))...)
			} else {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("nodeName"), *device.NodeName, "must not be empty"))
			}

		}
		if device.NodeSelector != nil {
			setFields = append(setFields, "`nodeSelector`")
			allErrs = append(allErrs, corevalidation.ValidateNodeSelector(device.NodeSelector, false, fldPath.Child("nodeSelector"))...)
		}
		if device.AllNodes != nil {
			if *device.AllNodes {
				setFields = append(setFields, "`allNodes`")
			} else {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("allNodes"), *device.AllNodes, "must be either unset or set to true"))
			}
		}
		switch len(setFields) {
		case 0:
			allErrs = append(allErrs, field.Required(fldPath, "exactly one of `nodeName`, `nodeSelector`, or `allNodes` is required when `perDeviceNodeSelection` is set to true in the ResourceSlice spec"))
		case 1:
		default:
			allErrs = append(allErrs, field.Invalid(fldPath, fmt.Sprintf("{%s}", strings.Join(setFields, ", ")), "exactly one of `nodeName`, `nodeSelector`, or `allNodes` is required when `perDeviceNodeSelection` is set to true in the ResourceSlice spec"))
		}
	} else if (perDeviceNodeSelection == nil || !*perDeviceNodeSelection) && (device.NodeName != nil || device.NodeSelector != nil || device.AllNodes != nil) {
		allErrs = append(allErrs, field.Invalid(fldPath, nil, "`nodeName`, `nodeSelector` and `allNodes` can only be set if `perDeviceNodeSelection` is set to true in the ResourceSlice spec"))
	}

	return allErrs
}

func validateDeviceCounterConsumption(deviceCounterConsumption resource.DeviceCounterConsumption, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	if len(deviceCounterConsumption.SharedCounter) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("sharedCounter"), ""))
	}
	if deviceCounterConsumption.Counters == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("counters"), ""))
	} else {
		allErrs = append(allErrs, validateMap(deviceCounterConsumption.Counters, resource.ResourceSliceMaxDeviceCounterConsumptionCounters, attributeAndCapacityMaxKeyLength,
			validateCounterName, validateDeviceCounter, fldPath.Child("counters"))...)
	}
	return allErrs
}

var (
	numericIdentifier = `(0|[1-9]\d*)`

	preReleaseIdentifier = `(0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)`

	buildIdentifier = `[0-9a-zA-Z-]+`

	semverRe = regexp.MustCompile(`^` +

		// dot-separated version segments (e.g. 1.2.3)
		numericIdentifier + `\.` + numericIdentifier + `\.` + numericIdentifier +

		// optional dot-separated prerelease segments (e.g. -alpha.PRERELEASE.1)
		`(-` + preReleaseIdentifier + `(\.` + preReleaseIdentifier + `)*)?` +

		// optional dot-separated build identifier segments (e.g. +build.id.20240305)
		`(\+` + buildIdentifier + `(\.` + buildIdentifier + `)*)?` +
		`$`)
)

func validateDeviceAttribute(attribute resource.DeviceAttribute, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	numFields := 0
	if attribute.BoolValue != nil {
		numFields++
	}
	if attribute.IntValue != nil {
		numFields++
	}
	if attribute.StringValue != nil {
		numFields++
		allErrs = append(allErrs, validateDeviceAttributeStringValue(attribute.StringValue, fldPath.Child("string"))...)
	}
	if attribute.VersionValue != nil {
		numFields++
		allErrs = append(allErrs, validateDeviceAttributeVersionValue(attribute.VersionValue, fldPath.Child("version"))...)
	}

	switch numFields {
	case 0:
		allErrs = append(allErrs, field.Required(fldPath, "exactly one value must be specified"))
	case 1:
		// Okay.
	default:
		allErrs = append(allErrs, field.Invalid(fldPath, attribute, "exactly one value must be specified"))
	}
	return allErrs
}

func validateDeviceAttributeStringValue(value *string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if len(*value) > resource.DeviceAttributeMaxValueLength {
		allErrs = append(allErrs, field.TooLong(fldPath, "" /*unused*/, resource.DeviceAttributeMaxValueLength))
	}
	return allErrs
}

func validateDeviceAttributeVersionValue(value *string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if !semverRe.MatchString(*value) {
		allErrs = append(allErrs, field.Invalid(fldPath, *value, "must be a string compatible with semver.org spec 2.0.0"))
	}
	if len(*value) > resource.DeviceAttributeMaxValueLength {
		allErrs = append(allErrs, field.TooLong(fldPath, "" /*unused*/, resource.DeviceAttributeMaxValueLength))
	}
	return allErrs
}

func validateDeviceCapacity(capacity resource.DeviceCapacity, fldPath *field.Path) field.ErrorList {
	// Any parsed quantity is valid.
	return nil
}

func validateDeviceCounter(counter resource.Counter, fldPath *field.Path) field.ErrorList {
	// Any parsed quantity is valid.
	return nil
}

func validateQualifiedName(name resource.QualifiedName, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if name == "" {
		allErrs = append(allErrs, field.Required(fldPath, "name required"))
		return allErrs
	}

	parts := strings.Split(string(name), "/")
	switch len(parts) {
	case 1:
		allErrs = append(allErrs, validateCIdentifier(parts[0], fldPath)...)
	case 2:
		if len(parts[0]) == 0 {
			allErrs = append(allErrs, field.Required(fldPath, "the domain must not be empty"))
		} else {
			allErrs = append(allErrs, validateDriverName(parts[0], fldPath)...)
		}
		if len(parts[1]) == 0 {
			allErrs = append(allErrs, field.Required(fldPath, "the name must not be empty"))
		} else {
			allErrs = append(allErrs, validateCIdentifier(parts[1], fldPath)...)
		}
	}
	return allErrs
}

func validateFullyQualifiedName(name resource.FullyQualifiedName, fldPath *field.Path) field.ErrorList {
	allErrs := validateQualifiedName(resource.QualifiedName(name), fldPath)
	// validateQualifiedName checks that the name isn't empty and both parts are valid.
	// What we need to enforce here is that there really is a domain.
	if name != "" && !strings.Contains(string(name), "/") {
		allErrs = append(allErrs, field.Invalid(fldPath, name, "must include a domain"))
	}
	return allErrs
}

func validateCIdentifier(id string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if len(id) > resource.DeviceMaxIDLength {
		allErrs = append(allErrs, field.TooLong(fldPath, "" /*unused*/, resource.DeviceMaxIDLength))
	}
	for _, msg := range validation.IsCIdentifier(id) {
		allErrs = append(allErrs, field.TypeInvalid(fldPath, id, msg))
	}
	return allErrs
}

// validateSlice ensures that a slice does not exceed a certain maximum size
// and that all entries are valid.
// A negative maxSize disables the length check.
func validateSlice[T any](slice []T, maxSize int, validateItem func(T, *field.Path) field.ErrorList, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	for i, item := range slice {
		idxPath := fldPath.Index(i)
		allErrs = append(allErrs, validateItem(item, idxPath)...)
	}
	if maxSize >= 0 && len(slice) > maxSize {
		// Dumping the entire field into the error message is likely to be too long,
		// in particular when it is already beyond the maximum size. Instead this
		// just shows the number of entries.
		allErrs = append(allErrs, field.TooMany(fldPath, len(slice), maxSize))
	}
	return allErrs
}

// validateSet ensures that a slice contains no duplicates, does not
// exceed a certain maximum size and that all entries are valid.
func validateSet[T any, K comparable](slice []T, maxSize int, validateItem func(item T, fldPath *field.Path) field.ErrorList, itemKey func(T) (K, string), fldPath *field.Path) field.ErrorList {
	allErrs := validateSlice(slice, maxSize, validateItem, fldPath)
	allItems := sets.New[K]()
	for i, item := range slice {
		idxPath := fldPath.Index(i)
		key, fieldName := itemKey(item)
		childPath := idxPath
		if fieldName != "" {
			childPath = childPath.Child(fieldName)
		}
		if allItems.Has(key) {
			allErrs = append(allErrs, field.Duplicate(childPath, key))
		} else {
			allItems.Insert(key)
		}
	}
	return allErrs
}

// stringKey uses the item itself as a key for validateSet.
func stringKey(item string) (string, string) {
	return item, ""
}

// validateMap validates keys, items and the maximum length of a map.
// A negative maxSize disables the length check.
//
// Keys larger than truncateKeyLen get truncated in the middle. A very
// small limit gets increased because it is okay to include more details.
// This is not used for validation of keys, which has to be done by
// the callback function.
func validateMap[K ~string, T any](m map[K]T, maxSize, truncateKeyLen int, validateKey func(K, *field.Path) field.ErrorList, validateItem func(T, *field.Path) field.ErrorList, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if maxSize >= 0 && len(m) > maxSize {
		allErrs = append(allErrs, field.TooMany(fldPath, len(m), maxSize))
	}
	for key, item := range m {
		keyPath := fldPath.Key(truncateIfTooLong(string(key), truncateKeyLen))
		allErrs = append(allErrs, validateKey(key, keyPath)...)
		allErrs = append(allErrs, validateItem(item, keyPath)...)
	}
	return allErrs
}

func truncateIfTooLong(str string, maxLen int) string {
	// The caller was overly restrictive. Increase the length to something reasonable
	// (https://github.com/kubernetes/kubernetes/pull/127511#discussion_r1826206362).
	if maxLen < 16 {
		maxLen = 16
	}
	if len(str) <= maxLen {
		return str
	}
	ellipsis := "..."
	remaining := maxLen - len(ellipsis)
	return str[0:(remaining+1)/2] + ellipsis + str[len(str)-remaining/2:]
}

func validateDeviceStatus(device resource.AllocatedDeviceStatus, fldPath *field.Path, allocatedDevices sets.Set[structured.DeviceID]) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateDriverName(device.Driver, fldPath.Child("driver"))...)
	allErrs = append(allErrs, validatePoolName(device.Pool, fldPath.Child("pool"))...)
	allErrs = append(allErrs, validateDeviceName(device.Device, fldPath.Child("device"))...)
	deviceID := structured.MakeDeviceID(device.Driver, device.Pool, device.Device)
	if !allocatedDevices.Has(deviceID) {
		allErrs = append(allErrs, field.Invalid(fldPath, deviceID, "must be an allocated device in the claim"))
	}
	if len(device.Conditions) > resource.AllocatedDeviceStatusMaxConditions {
		allErrs = append(allErrs, field.TooMany(fldPath.Child("conditions"), len(device.Conditions), resource.AllocatedDeviceStatusMaxConditions))
	}
	allErrs = append(allErrs, metav1validation.ValidateConditions(device.Conditions, fldPath.Child("conditions"))...)
	if device.Data != nil && len(device.Data.Raw) > 0 { // Data is an optional field.
		allErrs = append(allErrs, validateRawExtension(*device.Data, fldPath.Child("data"), false, resource.AllocatedDeviceStatusDataMaxLength)...)
	}
	allErrs = append(allErrs, validateNetworkDeviceData(device.NetworkData, fldPath.Child("networkData"))...)
	return allErrs
}

// validateRawExtension validates RawExtension as in https://github.com/kubernetes/kubernetes/pull/125549/
func validateRawExtension(rawExtension runtime.RawExtension, fldPath *field.Path, stored bool, rawExtensionMaxLength int) field.ErrorList {
	var allErrs field.ErrorList
	var v any
	if len(rawExtension.Raw) == 0 {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	} else if !stored && len(rawExtension.Raw) > rawExtensionMaxLength {
		// Don't even bother with parsing when too large.
		// Only applies on create. Existing parameters are grand-fathered in
		// because the limit was introduced in 1.32. This also means that it
		// can be changed in the future.
		allErrs = append(allErrs, field.TooLong(fldPath, "" /* unused */, rawExtensionMaxLength))
	} else if err := json.Unmarshal(rawExtension.Raw, &v); err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath, "<value omitted>", fmt.Sprintf("error parsing data as JSON: %v", err.Error())))
	} else if v == nil {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	} else if _, isObject := v.(map[string]any); !isObject {
		allErrs = append(allErrs, field.Invalid(fldPath, "<value omitted>", "parameters must be a valid JSON object"))
	}
	return allErrs
}

func validateNetworkDeviceData(networkDeviceData *resource.NetworkDeviceData, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if networkDeviceData == nil {
		return allErrs
	}

	if len(networkDeviceData.InterfaceName) > resource.NetworkDeviceDataInterfaceNameMaxLength {
		allErrs = append(allErrs, field.TooLong(fldPath.Child("interfaceName"), "" /* unused */, resource.NetworkDeviceDataInterfaceNameMaxLength))
	}

	if len(networkDeviceData.HardwareAddress) > resource.NetworkDeviceDataHardwareAddressMaxLength {
		allErrs = append(allErrs, field.TooLong(fldPath.Child("hardwareAddress"), "" /* unused */, resource.NetworkDeviceDataHardwareAddressMaxLength))
	}

	allErrs = append(allErrs, validateSet(networkDeviceData.IPs, resource.NetworkDeviceDataMaxIPs,
		func(address string, fldPath *field.Path) field.ErrorList {
			return validation.IsValidInterfaceAddress(fldPath, address)
		}, stringKey, fldPath.Child("ips"))...)
	return allErrs
}

// ValidateDeviceTaintRule tests if a DeviceTaintRule object is valid.
func ValidateDeviceTaintRule(deviceTaint *resource.DeviceTaintRule) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMeta(&deviceTaint.ObjectMeta, false, apimachineryvalidation.NameIsDNSSubdomain, field.NewPath("metadata"))
	allErrs = append(allErrs, validateDeviceTaintRuleSpec(&deviceTaint.Spec, nil, field.NewPath("spec"))...)
	return allErrs
}

// ValidateDeviceTaintRuleUpdate tests if a DeviceTaintRule update is valid.
func ValidateDeviceTaintRuleUpdate(deviceTaint, oldDeviceTaint *resource.DeviceTaintRule) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMetaUpdate(&deviceTaint.ObjectMeta, &oldDeviceTaint.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, validateDeviceTaintRuleSpec(&deviceTaint.Spec, &oldDeviceTaint.Spec, field.NewPath("spec"))...)
	return allErrs
}

func validateDeviceTaintRuleSpec(spec, oldSpec *resource.DeviceTaintRuleSpec, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	var oldFilter *resource.DeviceTaintSelector
	if oldSpec != nil {
		oldFilter = oldSpec.DeviceSelector // +k8s:verify-mutation:reason=clone
	}
	allErrs = append(allErrs, validateDeviceTaintSelector(spec.DeviceSelector, oldFilter, fldPath.Child("deviceSelector"))...)
	allErrs = append(allErrs, validateDeviceTaint(spec.Taint, fldPath.Child("taint"))...)
	return allErrs
}

func validateDeviceTaintSelector(filter, oldFilter *resource.DeviceTaintSelector, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	if filter == nil {
		return allErrs
	}
	if filter.DeviceClassName != nil {
		allErrs = append(allErrs, validateDeviceClassName(*filter.DeviceClassName, fldPath.Child("deviceClassName"))...)
	}
	if filter.Driver != nil {
		allErrs = append(allErrs, validateDriverName(*filter.Driver, fldPath.Child("driver"))...)
	}
	if filter.Pool != nil {
		allErrs = append(allErrs, validatePoolName(*filter.Pool, fldPath.Child("pool"))...)
	}
	if filter.Device != nil {
		allErrs = append(allErrs, validateDeviceName(*filter.Device, fldPath.Child("device"))...)
	}

	// If the selectors are exactly as before, we treat the CEL expressions as "stored".
	// Any change, including merely reordering selectors, triggers validation as new
	// expressions.
	stored := false
	if oldFilter != nil {
		stored = apiequality.Semantic.DeepEqual(filter.Selectors, oldFilter.Selectors)
	}
	allErrs = append(allErrs, validateSlice(filter.Selectors, resource.DeviceSelectorsMaxSize,
		func(selector resource.DeviceSelector, fldPath *field.Path) field.ErrorList {
			return validateSelector(selector, fldPath, stored)
		},
		fldPath.Child("selectors"))...)

	return allErrs
}

var validDeviceTolerationOperators = []resource.DeviceTolerationOperator{resource.DeviceTolerationOpEqual, resource.DeviceTolerationOpExists}
var validDeviceTaintEffects = sets.New(resource.DeviceTaintEffectNoSchedule, resource.DeviceTaintEffectNoExecute)

func validateDeviceTaint(taint resource.DeviceTaint, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	allErrs = append(allErrs, metav1validation.ValidateLabelName(taint.Key, fldPath.Child("key"))...) // Includes checking for non-empty.
	if taint.Value != "" {
		allErrs = append(allErrs, validateLabelValue(taint.Value, fldPath.Child("value"))...)
	}
	switch {
	case taint.Effect == "":
		allErrs = append(allErrs, field.Required(fldPath.Child("effect"), "")) // Required in a taint.
	case !validDeviceTaintEffects.Has(taint.Effect):
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("effect"), taint.Effect, sets.List(validDeviceTaintEffects)))
	}

	return allErrs
}

func validateDeviceToleration(toleration resource.DeviceToleration, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	if toleration.Key != "" {
		allErrs = append(allErrs, metav1validation.ValidateLabelName(toleration.Key, fldPath.Child("key"))...)
	}
	switch toleration.Operator {
	case resource.DeviceTolerationOpExists:
		if toleration.Value != "" {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("value"), toleration.Value, "must be empty for operator `Exists`"))
		}
	case resource.DeviceTolerationOpEqual:
		allErrs = append(allErrs, validateLabelValue(toleration.Value, fldPath.Child("value"))...)
	case "":
		allErrs = append(allErrs, field.Required(fldPath.Child("operator"), ""))
	default:
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("operator"), toleration.Operator, validDeviceTolerationOperators))
	}
	switch {
	case toleration.Effect == "":
		// Optional in a toleration.
	case !validDeviceTaintEffects.Has(toleration.Effect):
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("effect"), toleration.Effect, sets.List(validDeviceTaintEffects)))
	}

	return allErrs
}

func validateLabelValue(value string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	// There's no metav1validation.ValidateLabelValue.
	for _, msg := range validation.IsValidLabelValue(value) {
		allErrs = append(allErrs, field.Invalid(fldPath, value, msg))
	}

	return allErrs
}
