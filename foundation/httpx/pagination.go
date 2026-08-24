package httpx

import (
	"github.com/fairlb/fairlb/foundation/cursorpage"
	"github.com/fairlb/fairlb/foundation/errcode"
)

func ParseCursorPage(cursor *string, limit *int, def, max int) (cursorpage.Page, error) {
	page, err := cursorpage.Parse(cursor, limit, def, max)
	if err != nil {
		return cursorpage.Page{}, ErrCodeDetail(errcode.CommonValidation, "Invalid cursor")
	}
	return page, nil
}

// ParseKeyPage is ParseCursorPage's sibling for lists ordered by a text key
// rather than by time.
//
// Same refusal, deliberately: every malformed cursor is one 400 saying "that
// cursor is not valid". A wrong-arity cursor is a caller mixing up two
// endpoints, and telling them which component count was expected would describe
// the internals of an opaque token.
func ParseKeyPage(cursor *string, limit *int, parts, def, max int) (cursorpage.KeyPage, error) {
	page, err := cursorpage.ParseKeyPage(cursor, limit, parts, def, max)
	if err != nil {
		return cursorpage.KeyPage{}, ErrCodeDetail(errcode.CommonValidation, "Invalid cursor")
	}
	return page, nil
}
