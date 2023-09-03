package xerrors

import (
	"errors"
	"fmt"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Issue"

	"github.com/ydb-platform/ydb-go-sdk/v3/internal/allocator"
	"github.com/ydb-platform/ydb-go-sdk/v3/internal/backoff"
)

// operationError reports about operationStatus fail.
type operationError struct {
	code    Ydb.StatusIds_StatusCode
	issues  issues
	address string
}

func (e *operationError) isYdbError() {}

func (e *operationError) Code() int32 {
	return int32(e.code)
}

func (e *operationError) Name() string {
	return "operation/" + e.code.String()
}

type operationStatus interface {
	GetStatus() Ydb.StatusIds_StatusCode
	GetIssues() []*Ydb_Issue.IssueMessage
}

// WithIssues is an option for construct operationStatus error with issues list
// WithIssues must use as `Operation(WithIssues(issues))`
func WithIssues(issues []*Ydb_Issue.IssueMessage) oeOpt {
	return func(oe *operationError) {
		oe.issues = issues
	}
}

// WithStatusCode is an option for construct operationStatus error with reason code
// WithStatusCode must use as `Operation(WithStatusCode(reason))`
func WithStatusCode(code Ydb.StatusIds_StatusCode) oeOpt {
	return func(oe *operationError) {
		oe.code = code
	}
}

// WithNodeAddress is an option for construct operationStatus error with node address
// WithNodeAddress must use as `Operation(WithNodeAddress(reason))`
func WithNodeAddress(address string) oeOpt {
	return func(oe *operationError) {
		oe.address = address
	}
}

// FromOperation is an option for construct operationStatus error from operationStatus
// FromOperation must use as `Operation(FromOperation(operationStatus))`
func FromOperation(operation operationStatus) oeOpt {
	return func(oe *operationError) {
		oe.code = operation.GetStatus()
		oe.issues = operation.GetIssues()
	}
}

type oeOpt func(ops *operationError)

func Operation(opts ...oeOpt) error {
	oe := &operationError{
		code: Ydb.StatusIds_STATUS_CODE_UNSPECIFIED,
	}
	for _, f := range opts {
		if f != nil {
			f(oe)
		}
	}
	return oe
}

func (e *operationError) Issues() []*Ydb_Issue.IssueMessage {
	return e.issues
}

func (e *operationError) Error() string {
	b := allocator.Buffers.Get()
	defer allocator.Buffers.Put(b)
	b.WriteString(e.Name())
	fmt.Fprintf(b, " (code = %d", e.code)
	if len(e.address) > 0 {
		b.WriteString(", address = ")
		b.WriteString(e.address)
	}
	if len(e.issues) > 0 {
		b.WriteString(", issues = ")
		b.WriteString(e.issues.String())
	}
	b.WriteString(")")
	return b.String()
}

// IsOperationError reports whether err is operationError with given errType codes.
func IsOperationError(err error, codes ...Ydb.StatusIds_StatusCode) bool {
	var op *operationError
	if !errors.As(err, &op) {
		return false
	}
	if len(codes) == 0 {
		return true
	}
	for _, code := range codes {
		if op.code == code {
			return true
		}
	}
	return false
}

const issueCodeTransactionLocksInvalidated = 2001

func IsOperationErrorTransactionLocksInvalidated(err error) (isTLI bool) {
	if IsOperationError(err, Ydb.StatusIds_ABORTED) {
		IterateByIssues(err, func(_ string, code Ydb.StatusIds_StatusCode, severity uint32) {
			isTLI = isTLI || (code == issueCodeTransactionLocksInvalidated)
		})
	}
	return isTLI
}

func (e *operationError) Type() Type {
	switch e.code {
	case
		Ydb.StatusIds_ABORTED,
		Ydb.StatusIds_UNAVAILABLE,
		Ydb.StatusIds_OVERLOADED,
		Ydb.StatusIds_BAD_SESSION,
		Ydb.StatusIds_SESSION_BUSY:
		return TypeRetryable
	case Ydb.StatusIds_UNDETERMINED:
		return TypeConditionallyRetryable
	default:
		return TypeUndefined
	}
}

func (e *operationError) BackoffType() backoff.Type {
	switch e.code {
	case Ydb.StatusIds_OVERLOADED:
		return backoff.TypeSlow
	case
		Ydb.StatusIds_ABORTED,
		Ydb.StatusIds_UNAVAILABLE,
		Ydb.StatusIds_CANCELLED,
		Ydb.StatusIds_SESSION_BUSY,
		Ydb.StatusIds_UNDETERMINED:
		return backoff.TypeFast
	default:
		return backoff.TypeNoBackoff
	}
}

func (e *operationError) MustDeleteSession() bool {
	switch e.code {
	case
		Ydb.StatusIds_BAD_SESSION,
		Ydb.StatusIds_SESSION_EXPIRED,
		Ydb.StatusIds_SESSION_BUSY:
		return true
	default:
		return false
	}
}

func OperationError(err error) Error {
	var o *operationError
	if errors.As(err, &o) {
		return o
	}
	return nil
}
