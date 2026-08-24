package gwstaffapi

import (
	"errors"
	"fmt"
	"strings"

	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/pricing"
)

// The pricing application service.
//
// 这里此前有五个接口（PricingAdminService 及其三个组成部分、ModelMarginSummaryService），
// 全部由同包内唯一的 *pgPricingAdminService 满足。注释里写着它们存在的理由是「让 handler
// 单测不需要数据库就能跑」——而 NewServer 无条件构造 PG 实现、注入器早已删除、字段不可导出，
// 三处用到它的测试用的都是 NewPGPricingAdminService 加真实 pool。**那个理由不可达**（ADR-0157）。
// 定价写路径的领域错误。pricingHTTPError 把它们映射到 HTTP。
var (
	ErrPricingNotFound   = errors.New("pricing resource not found")
	ErrPricingConflict   = errors.New("pricing resource revision conflict")
	ErrPricingReferenced = errors.New("pricing resource is still referenced")
	ErrPricingInvalid    = errors.New("invalid pricing operation")
)

// mapPricingDomainError turns the pricing domain's errors into this package's.
//
// Two vocabularies rather than one, on purpose: the domain says "not found",
// the transport decides that means 404. Collapsing them would put HTTP status
// codes inside the pricing rules, which is the coupling this move removed.
func mapPricingDomainError(err error) error {
	switch {
	case errors.Is(err, pricing.ErrNotFound):
		return ErrPricingNotFound
	case errors.Is(err, pricing.ErrConflict):
		return ErrPricingConflict
	case errors.Is(err, pricing.ErrInvalid):
		// Wrapped, not replaced: the domain's message names the field or the
		// risk, and that text is the whole value of a 4xx over a 500 here.
		return fmt.Errorf("%w: %s", ErrPricingInvalid,
			strings.TrimPrefix(err.Error(), pricing.ErrInvalid.Error()+": "))
	default:
		return err
	}
}

func pricingHTTPError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrPricingNotFound), db.IsNoRows(err):
		return httpx.ErrCodeDetail(errcode.CommonNotFound, "Pricing resource not found")
	case errors.Is(err, ErrPricingConflict):
		return httpx.ErrCodeDetail(errcode.CommonPreconditionFailed, "This resource was modified by someone else; reload and retry")
	case errors.Is(err, ErrPricingReferenced):
		return httpx.ErrCodeDetail(errcode.CommonConflict, err.Error())
	case errors.Is(err, ErrPricingInvalid):
		return httpx.ErrCodeDetail(errcode.CommonValidation, err.Error())
	default:
		return err
	}
}
