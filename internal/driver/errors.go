package driver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.origincloud.eu/sdk/core"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// apiStatus turns an error from the Origin Cloud API into the gRPC status
// the CSI caller acts on. what says what the driver was doing, for the
// message. The mapping follows the platform's error reasons, which the API
// documents as stable, not the HTTP status:
//
//   - NOT_FOUND: NotFound.
//   - CONFLICT: FailedPrecondition. The resource is in a state that refuses
//     the call (a volume attached elsewhere, one still in use); the CO
//     retries once that changed.
//   - INSUFFICIENT_CAPACITY, an exhausted quota (BAD_REQUEST on the field
//     `quota`) and any other answer whose status is RESOURCE_EXHAUSTED:
//     ResourceExhausted.
//   - BAD_REQUEST: InvalidArgument.
//   - UNAUTHORIZED: Unauthenticated. FORBIDDEN: PermissionDenied.
//   - SERVICE_UNAVAILABLE: Unavailable.
//   - PRODUCT_UNAVAILABLE, PAYMENT_METHOD_REQUIRED,
//     LEGAL_ACCEPTANCE_REQUIRED: FailedPrecondition: the account has to act.
//   - the context ending: DeadlineExceeded or Canceled; the CO retries,
//     and the retry finds what the first attempt started.
//   - anything else: Internal.
func apiStatus(err error, what string) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok && !isAPIError(err) {
		return err
	}
	msg := fmt.Sprintf("%s: %v", what, err)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, msg)
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, msg)
	}
	var apiErr *core.Error
	if !errors.As(err, &apiErr) {
		return status.Error(codes.Internal, msg)
	}
	return status.Error(codeOf(apiErr), msg)
}

func isAPIError(err error) bool {
	var apiErr *core.Error
	return errors.As(err, &apiErr)
}

func codeOf(e *core.Error) codes.Code {
	switch e.Reason {
	case "NOT_FOUND":
		return codes.NotFound
	case "CONFLICT", "PRODUCT_UNAVAILABLE", "PAYMENT_METHOD_REQUIRED", "LEGAL_ACCEPTANCE_REQUIRED":
		return codes.FailedPrecondition
	case "INSUFFICIENT_CAPACITY", "RESOURCE_EXHAUSTED":
		return codes.ResourceExhausted
	case "BAD_REQUEST":
		if quotaExceeded(e) {
			return codes.ResourceExhausted
		}
		return codes.InvalidArgument
	case "UNAUTHORIZED":
		return codes.Unauthenticated
	case "FORBIDDEN":
		return codes.PermissionDenied
	case "SERVICE_UNAVAILABLE":
		return codes.Unavailable
	}
	// No reason the driver knows: fall back on the canonical status.
	switch e.Status {
	case "NOT_FOUND":
		return codes.NotFound
	case "RESOURCE_EXHAUSTED":
		return codes.ResourceExhausted
	case "FAILED_PRECONDITION":
		return codes.FailedPrecondition
	case "INVALID_ARGUMENT":
		return codes.InvalidArgument
	case "PERMISSION_DENIED":
		return codes.PermissionDenied
	case "UNAUTHENTICATED":
		return codes.Unauthenticated
	case "UNAVAILABLE":
		return codes.Unavailable
	}
	return codes.Internal
}

// quotaExceeded reports whether a BAD_REQUEST refuses the call for an
// exhausted quota, which the API reports on the field `quota`.
func quotaExceeded(e *core.Error) bool {
	for _, v := range e.FieldViolations {
		if v.Field == "quota" {
			return true
		}
	}
	return strings.Contains(e.Message, "on field quota:")
}

// isNotFound reports whether the API answered NOT_FOUND.
func isNotFound(err error) bool { return core.ReasonOf(err) == "NOT_FOUND" }
