package gwstaffapi

import (
	"errors"
	"fmt"
	"testing"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
)

func TestPricingHTTPErrorSeparatesRevisionAndReferenceConflicts(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		code   string
		status int
	}{
		{
			name:   "stale If-Match is a failed precondition",
			err:    ErrPricingConflict,
			code:   errcode.CommonPreconditionFailed,
			status: 412,
		},
		{
			name:   "referenced plan is a resource-state conflict",
			err:    fmt.Errorf("%w: still assigned", ErrPricingReferenced),
			code:   errcode.CommonConflict,
			status: 409,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *httpx.CodeError
			if err := pricingHTTPError(tt.err); !errors.As(err, &got) {
				t.Fatalf("pricingHTTPError() = %T %v, want *httpx.CodeError", err, err)
			}
			if got.Code != tt.code {
				t.Fatalf("code = %q, want %q", got.Code, tt.code)
			}
			def, ok := errcode.Lookup(got.Code)
			if !ok {
				t.Fatalf("%s 未注册——错误码是对外契约的一部分，走到这里说明生成链出了问题", got.Code)
			}
			if def.Status != tt.status {
				t.Fatalf("status = %d, want %d", def.Status, tt.status)
			}
		})
	}
}
