package vulcan

import (
	timetools "github.com/mailgun/gotools-time"
	. "github.com/mailgun/vulcan/location"
	. "github.com/mailgun/vulcan/route"
	. "github.com/mailgun/vulcan/testutils"
	. "launchpad.net/gocheck"
	"net/http"
	"net/http/httptest"
	"time"
)

type ProxySuite struct {
	authHeaders http.Header
	tm          *timetools.FreezedTime
}

var _ = Suite(&ProxySuite{
	tm: &timetools.FreezedTime{
		CurrentTime: time.Date(2012, 3, 4, 5, 6, 7, 0, time.UTC),
	},
})

// Success, make sure we've successfully proxied the response
func (s *ProxySuite) TestSuccess(c *C) {
	server := NewTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Hi, I'm endpoint"))
	})
	defer server.Close()

	proxy, err := NewProxy(&ConstRouter{&ConstHttpLocation{server.URL}})
	c.Assert(err, IsNil)
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	response, bodyBytes := Get(c, proxyServer.URL, nil, "hello!")
	c.Assert(response.StatusCode, Equals, http.StatusOK)
	c.Assert(string(bodyBytes), Equals, "Hi, I'm endpoint")
}

func (s *ProxySuite) TestFailure(c *C) {
	proxy, err := NewProxy(&ConstRouter{&ConstHttpLocation{"http://localhost:63999"}})
	c.Assert(err, IsNil)
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	response, _ := Get(c, proxyServer.URL, nil, "hello!")
	c.Assert(response.StatusCode, Equals, http.StatusBadGateway)
}
