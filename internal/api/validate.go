// Copyright 2025 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api

import (
	"context"
	"crypto/x509"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	validator "github.com/go-playground/validator/v10"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"

	"github.com/Azure/ARO-HCP/internal/api/arm"
)

type contextKey int

const (
	contextKeyRequest contextKey = iota
	contextKeyResourceType
)

// GetJSONTagName extracts the JSON field name from the "json" key in
// a struct tag. Returns an empty string if no "json" key is present,
// or if the value is "-".
func GetJSONTagName(tag reflect.StructTag) string {
	tagValue := tag.Get("json")
	if tagValue == "-" {
		return ""
	}
	fieldName, _, _ := strings.Cut(tagValue, ",")
	return fieldName
}

// EnumValidateTag generates a string suitable for use with the "validate"
// struct tag. The intent is to convert a set of valid values for a string
// subtype into a "oneof=" expression for the purpose of static validation.
func EnumValidateTag[S ~string](values ...S) string {
	s := make([]string, len(values))
	for i, e := range values {
		s[i] = string(e)
		// Replace special characters with the UTF-8 hex representation.
		// https://pkg.go.dev/github.com/go-playground/validator/v10#hdr-Using_Validator_Tags
		s[i] = strings.ReplaceAll(s[i], ",", "0x2C")
		s[i] = strings.ReplaceAll(s[i], "|", "0x7C")
	}
	return fmt.Sprintf("oneof=%s", strings.Join(s, " "))
}

func NewValidator() *validator.Validate {
	var err error

	validate := validator.New(validator.WithRequiredStructEnabled())

	// Use "json" struct tags for alternate field names.
	// Alternate field names will be used in validation errors.
	validate.RegisterTagNameFunc(func(field reflect.StructField) string {
		return GetJSONTagName(field.Tag)
	})

	// Register ARM-mandated enumeration types.
	validate.RegisterAlias("enum_managedserviceidentitytype", EnumValidateTag(
		arm.ManagedServiceIdentityTypeNone,
		arm.ManagedServiceIdentityTypeSystemAssigned,
		arm.ManagedServiceIdentityTypeSystemAssignedUserAssigned,
		arm.ManagedServiceIdentityTypeUserAssigned))
	validate.RegisterAlias("enum_subscriptionstate", EnumValidateTag(
		arm.SubscriptionStateRegistered,
		arm.SubscriptionStateUnregistered,
		arm.SubscriptionStateWarned,
		arm.SubscriptionStateDeleted,
		arm.SubscriptionStateSuspended))

	// Use this for string fields specifying an ARO-HCP API version.
	err = validate.RegisterValidation("api_version", func(fl validator.FieldLevel) bool {
		field := fl.Field()
		if field.Kind() != reflect.String {
			panic("String type required for api_version")
		}
		_, ok := Lookup(field.String())
		return ok
	})
	if err != nil {
		panic(err)
	}

	// Use this for string fields that must be a valid Kubernetes qualified name.
	err = validate.RegisterValidation("k8s_qualified_name", func(fl validator.FieldLevel) bool {
		field := fl.Field()
		if field.Kind() != reflect.String {
			panic("String type required for k8s_qualified_name")
		}
		return len(k8svalidation.IsQualifiedName(field.String())) == 0
	})
	if err != nil {
		panic(err)
	}

	// Use this for string fields that must be a valid Kubernetes label value.
	err = validate.RegisterValidation("k8s_label_value", func(fl validator.FieldLevel) bool {
		field := fl.Field()
		if field.Kind() != reflect.String {
			panic("String type required for k8s_label_value")
		}
		return len(k8svalidation.IsValidLabelValue(field.String())) == 0
	})
	if err != nil {
		panic(err)
	}

	// Use this for version ID fields that might begin with "openshift-v".
	err = validate.RegisterValidation("openshift_version", func(fl validator.FieldLevel) bool {
		field := fl.Field()
		if field.Kind() != reflect.String {
			panic("String type required for openshift_version")
		}
		_, err := NewOpenShiftVersion(field.String())
		return err == nil
	})
	if err != nil {
		panic(err)
	}

	// Use this for string fields providing PEM encoded certificates.
	err = validate.RegisterValidation("pem_certificates", func(fl validator.FieldLevel) bool {
		field := fl.Field()
		if field.Kind() != reflect.String {
			panic("String type required for pem_certificates")
		}
		return x509.NewCertPool().AppendCertsFromPEM([]byte(field.String()))
	})
	if err != nil {
		panic(err)
	}

	// Use this for fields required in PUT requests. Do not apply to read-only fields.
	err = validate.RegisterValidationCtx("required_for_put", func(ctx context.Context, fl validator.FieldLevel) bool {
		var method string

		if request, ok := ctx.Value(contextKeyRequest).(*http.Request); ok {
			if request != nil {
				method = request.Method
			}
		} else {
			panic(fmt.Sprintf("Could not obtain http.Request for %q validation", fl.GetTag()))
		}

		switch method {
		case http.MethodPut:
			// proceed
		case http.MethodPost:
			// For deployment preflight we evaluate resources as though it were a PUT.
			resourceType, ok := ctx.Value(contextKeyResourceType).(azcorearm.ResourceType)
			if !ok || !strings.EqualFold(resourceType.String(), PreflightResourceType.String()) {
				return true
			}
		default:
			return true
		}

		// This is replicating the implementation of "required".
		// See https://github.com/go-playground/validator/issues/492
		// Sounds like "hasValue" is unlikely to be exported and
		// "validate.Var" does not seem like a safe alternative.
		field := fl.Field()
		_, kind, nullable := fl.ExtractType(field)
		switch kind {
		case reflect.Slice, reflect.Map, reflect.Ptr, reflect.Interface, reflect.Chan, reflect.Func:
			return !field.IsNil()
		default:
			if nullable && field.Interface() != nil {
				return true
			}
			return field.IsValid() && !field.IsZero()
		}
	})
	if err != nil {
		panic(err)
	}

	// Use this for string fields specifying an Azure resource ID.
	// The optional argument further enforces a specific resource type.
	err = validate.RegisterValidation("resource_id", func(fl validator.FieldLevel) bool {
		field := fl.Field()
		param := fl.Param()
		if field.Kind() != reflect.String {
			panic("String type required for resource_id")
		}
		resourceID, err := azcorearm.ParseResourceID(field.String())
		if err != nil {
			return false
		}
		// Check for required fields.
		if resourceID.SubscriptionID == "" {
			return false
		}
		if resourceID.ResourceGroupName == "" {
			return false
		}
		if resourceID.Name == "" {
			return false
		}
		resourceType := resourceID.ResourceType.String()
		return param == "" || strings.EqualFold(resourceType, param)
	})
	if err != nil {
		panic(err)
	}

	return validate
}

// fieldNameToJSONName returns the JSON tag name for fieldName using a fieldError
// instance for type T. The fieldName should be a parameter value from fieldError.
// If the JSON tag name cannot be found, such as if the fieldError corresponds to
// a different type than type T, then fieldNameToJSONName just returns fieldName.
func fieldNameToJSONName[T any](fe validator.FieldError, fieldName string) string {
	reflectType := reflect.TypeFor[T]()
	if reflectType.Kind() == reflect.Pointer {
		reflectType = reflectType.Elem()
	}

	namespaceSegments := strings.Split(fe.StructNamespace(), ".")

	// First namespace segment should be the name of type T.
	if len(namespaceSegments) < 2 || namespaceSegments[0] != reflectType.Name() {
		return fieldName
	}

	for _, segment := range namespaceSegments[1 : len(namespaceSegments)-1] {
		if reflectType.Kind() != reflect.Struct {
			return fieldName
		}

		// Discard any subscript in the segment.
		index := strings.Index(segment, "[")
		if index >= 0 {
			segment = segment[:index]
		}

		if field, ok := reflectType.FieldByName(segment); ok {
			switch field.Type.Kind() {
			case reflect.Map, reflect.Pointer, reflect.Slice:
				reflectType = field.Type.Elem()
			default:
				reflectType = field.Type
			}
		} else {
			return fieldName
		}
	}

	if reflectType.Kind() == reflect.Struct {
		if field, ok := reflectType.FieldByName(fieldName); ok {
			return GetJSONTagName(field.Tag)
		}
	}

	return fieldName
}

// fieldErrorToTarget converts a validator.FieldError to a string suitable
// for use as a CloudErrorBody.Target by removing leading namespace segments
// that have no JSON tag (struct name + any embedded structs).
//
// e.g. "HCPOpenShiftCluster.TrackedResource.Resource.name" shortens to "name"
// because the Resource.Name field has a JSON tag but the rest of the namespace
// segments do not.
func fieldErrorToTarget(fe validator.FieldError) string {
	// These segments use the JSON field name if present.
	namespace := strings.Split(fe.Namespace(), ".")
	// These segments use only the struct field name.
	structNamespace := strings.Split(fe.StructNamespace(), ".")

	// Find the index where namespace and structNamespace diverge.
	minLength := min(len(namespace), len(structNamespace))
	for i := 0; i < minLength; i++ {
		if namespace[i] != structNamespace[i] {
			return strings.Join(namespace[i:], ".")
		}
	}

	// Fallback in case none of the namespace segments have JSON names.
	return fe.Namespace()
}

func ValidateRequest[T any](validate *validator.Validate, request *http.Request, resource T) []arm.CloudErrorBody {
	var ctx = context.Background()
	var errorDetails []arm.CloudErrorBody

	ctx = context.WithValue(ctx, contextKeyRequest, request)
	if request != nil && request.URL != nil {
		resourceType, err := azcorearm.ParseResourceType(request.URL.Path)
		if err == nil {
			ctx = context.WithValue(ctx, contextKeyResourceType, resourceType)
		}
	}
	err := validate.StructCtx(ctx, resource)

	if err == nil {
		return nil
	}

	// Convert validation errors to cloud error details.
	switch err := err.(type) {
	case validator.ValidationErrors:
		for _, fieldErr := range err {
			message := fmt.Sprintf("Invalid value '%v' for field '%s'", fieldErr.Value(), fieldErr.Field())
			// Try to add a corrective suggestion to the message.
			tag := fieldErr.Tag()
			params := strings.Fields(fieldErr.Param())
			if strings.HasPrefix(tag, "enum_") {
				if len(params) == 1 {
					message += fmt.Sprintf(" (must be %s)", params[0])
				} else {
					message += fmt.Sprintf(" (must be one of: %s)", strings.Join(params, " "))
				}
			} else {
				switch tag {
				case "api_version": // custom tag
					message = fmt.Sprintf("Unrecognized API version '%s'", fieldErr.Value())
				case "openshift_version": // custom tag
					message = fmt.Sprintf("Invalid OpenShift version '%s'", fieldErr.Value())
				case "pem_certificates": // custom tag
					message += " (must provide PEM encoded certificates)"
				case "k8s_label_value": // custom tag
					// Rerun the label value validation to obtain the error message.
					if value, ok := fieldErr.Value().(string); ok {
						errList := k8svalidation.IsValidLabelValue(value)
						message += fmt.Sprintf(" (%s)", strings.Join(errList, "; "))
					}
				case "k8s_qualified_name": // custom tag
					// Rerun the qualified name validation to obtain the error message.
					if value, ok := fieldErr.Value().(string); ok {
						errList := k8svalidation.IsQualifiedName(value)
						message += fmt.Sprintf(" (%s)", strings.Join(errList, "; "))
					}
				case "required", "required_for_put": // custom tag
					message = fmt.Sprintf("Missing required field '%s'", fieldErr.Field())
				case "required_unless":
					// The parameter format is pairs of "fieldName fieldValue".
					// Multiple pairs are possible but we currently only use one.
					if len(params) > 1 {
						jsonName := fieldNameToJSONName[T](fieldErr, params[0])
						message = fmt.Sprintf("Field '%s' is required when '%s' is not '%s'", fieldErr.Field(), jsonName, params[1])
					}
				case "resource_id": // custom tag
					if len(params) > 0 {
						message += fmt.Sprintf(" (must be a valid '%s' resource ID)", params[0])
					} else {
						message += " (must be a valid Azure resource ID)"
					}
				case "cidrv4":
					message += " (must be a v4 CIDR range)"
				case "dns_rfc1035_label":
					message += " (must be a valid DNS RFC 1035 label)"
				case "excluded_with":
					if len(params) > 0 {
						zero := reflect.Zero(fieldErr.Type()).Interface()
						jsonName := fieldNameToJSONName[T](fieldErr, params[0])
						message = fmt.Sprintf("Field '%s' must be %v when '%s' is specified", fieldErr.Field(), zero, jsonName)
					}
				case "gtefield":
					if len(params) > 0 {
						jsonName := fieldNameToJSONName[T](fieldErr, params[0])
						message += fmt.Sprintf(" (must be at least the value of '%s')", jsonName)
					}
				case "ipv4":
					message += " (must be an IPv4 address)"
				case "max":
					if len(params) > 0 {
						switch fieldErr.Kind() {
						case reflect.String:
							message += fmt.Sprintf(" (maximum length is %s)", params[0])
						default:
							if params[0] == "0" {
								message += " (must be non-positive)"
							} else {
								message += fmt.Sprintf(" (must be at most %s)", params[0])
							}
						}
					}
				case "min":
					if len(params) > 0 {
						switch fieldErr.Kind() {
						case reflect.String:
							message += fmt.Sprintf(" (minimum length is %s)", params[0])
						default:
							if params[0] == "0" {
								message += " (must be non-negative)"
							} else {
								message += fmt.Sprintf(" (must be at least %s)", params[0])
							}
						}
					}
				case "startswith":
					if len(params) > 0 {
						message += fmt.Sprintf(" (must start with '%s')", params[0])
					}
				case "url":
					message += " (must be a URL)"
				}
			}
			errorDetails = append(errorDetails, arm.CloudErrorBody{
				Code:    arm.CloudErrorCodeInvalidRequestContent,
				Message: message,
				Target:  fieldErrorToTarget(fieldErr),
			})
		}
	default:
		errorDetails = append(errorDetails, arm.CloudErrorBody{
			Code:    arm.CloudErrorCodeInvalidRequestContent,
			Message: err.Error(),
		})
	}

	return errorDetails
}

// ValidateSubscription validates a subscription request payload.
func ValidateSubscription(subscription *arm.Subscription, request *http.Request) *arm.CloudError {
	errorDetails := ValidateRequest(NewValidator(), request, subscription)

	// Returns nil if errorDetails is empty.
	return arm.NewContentValidationError(errorDetails)
}
