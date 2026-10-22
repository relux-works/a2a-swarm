package matrix

import (
	"context"
	"net/http"
)

// GetAccountData reads the global account data of type typ for userID into
// out (GET /user/{userId}/account_data/{type}). A type that was never set is a
// 404 M_NOT_FOUND *Error.
func (c *Client) GetAccountData(ctx context.Context, userID, typ string, out any) error {
	return c.callBulk(ctx, http.MethodGet, v3+"/user/"+seg(userID)+"/account_data/"+seg(typ), nil, nil, out)
}

// PutAccountData replaces the global account data of type typ for userID
// (PUT /user/{userId}/account_data/{type}).
func (c *Client) PutAccountData(ctx context.Context, userID, typ string, content any) error {
	return c.call(ctx, http.MethodPut, v3+"/user/"+seg(userID)+"/account_data/"+seg(typ), nil, content, nil)
}
