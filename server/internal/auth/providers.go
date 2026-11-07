package auth

import (
	"github.com/labstack/echo/v4"
	"github.com/markbates/goth"
	"github.com/markbates/goth/gothic"
	"github.com/markbates/goth/providers/github"
	"github.com/markbates/goth/providers/google"
)

// These two wrappers exist so this file is the only place in the repo that names a goth
// provider package. Adding a provider is: one wrapper here, one case in newProvider, one secret
// in the deploy config.

func githubProvider(clientID, secret, callback string, scopes ...string) goth.Provider {
	return github.New(clientID, secret, callback, scopes...)
}

func googleProvider(clientID, secret, callback string, scopes ...string) goth.Provider {
	return google.New(clientID, secret, callback, scopes...)
}

// oauthFlow isolates the gothic calls. gothic's API is set around http.ResponseWriter and
// *http.Request; keeping that in one small type means the handlers above read as normal echo
// handlers and the gothic dependency is testable in isolation.
type oauthFlow struct {
	cookieName string
	dev        bool
}

func newOAuthFlow(cookieName string, dev bool) *oauthFlow {
	return &oauthFlow{cookieName: cookieName, dev: dev}
}

// begin writes the signed state cookie and redirects to the provider.
func (f *oauthFlow) begin(c echo.Context, provider string) {
	req := gothic.GetContextWithProvider(c.Request(), provider)
	// gothic writes the redirect itself. Using its handler keeps the state generation and
	// the cookie write inside the library that later validates them.
	gothic.BeginAuthHandler(c.Response(), req)
}

// complete verifies the state and exchanges the code for a user. gothic deletes the state
// cookie on success (`defer Logout`), which is what makes a callback single-use.
func (f *oauthFlow) complete(c echo.Context, provider string) (goth.User, error) {
	req := gothic.GetContextWithProvider(c.Request(), provider)
	return gothic.CompleteUserAuth(c.Response(), req)
}

// clear removes the state cookie after a failed callback. gothic only clears it on the success
// path, and a stale state cookie is exactly the kind of thing that turns a retry into a
// confusing failure ten minutes later.
func (f *oauthFlow) clear(c echo.Context) {
	_ = gothic.Logout(c.Response(), c.Request())
}
