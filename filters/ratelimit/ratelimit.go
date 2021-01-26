/*
Package ratelimit provides filters to control the rate limitter settings on the route level.

For detailed documentation of the ratelimit, see https://godoc.org/github.com/zalando/skipper/ratelimit.
*/
package ratelimit

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/zalando/skipper/filters"
	"github.com/zalando/skipper/ratelimit"
)

type spec struct {
	typ        ratelimit.RatelimitType
	provider   RatelimitProvider
	filterName string
}

type filter struct {
	mu         sync.Mutex
	settings   ratelimit.Settings
	provider   RatelimitProvider
	overwrites map[string]ratelimit.Settings
}

// RatelimitProvider returns a limit instance for provided Settings
type RatelimitProvider interface {
	get(s ratelimit.Settings) limit
}

type limit interface {
	// AllowContext is used to decide if call is allowed to pass
	AllowContext(context.Context, string) bool

	// RetryAfter is used to inform the client how many seconds it
	// should wait before making a new request
	RetryAfter(string) int
}

// RegistryAdapter adapts ratelimit.Registry to RateLimitProvider interface.
// ratelimit.Registry is not an interface and its Get method returns
// ratelimit.Ratelimit which is not an interface either
// RegistryAdapter narrows ratelimit interfaces to necessary minimum
// and enables easier test stubbing
type registryAdapter struct {
	registry *ratelimit.Registry
}

func (a *registryAdapter) get(s ratelimit.Settings) limit {
	return a.registry.Get(s)
}

func NewRatelimitProvider(registry *ratelimit.Registry) RatelimitProvider {
	return &registryAdapter{registry}
}

// NewLocalRatelimit is *DEPRECATED*, use NewClientRatelimit, instead
func NewLocalRatelimit(provider RatelimitProvider) filters.Spec {
	return &spec{typ: ratelimit.LocalRatelimit, provider: provider, filterName: ratelimit.LocalRatelimitName}
}

// NewClientRatelimit creates a instance based client rate limit.  If
// you have 5 instances with 20 req/s, then it would allow 100 req/s
// to the backend from the same client. A third argument can be used to
// set which HTTP header of the request should be used to find the
// same user. Third argument defaults to XForwardedForLookuper,
// meaning X-Forwarded-For Header.
//
// Example:
//
//    backendHealthcheck: Path("/healthcheck")
//    -> clientRatelimit(20, "1m")
//    -> "https://foo.backend.net";
//
// Example rate limit per Authorization Header:
//
//    login: Path("/login")
//    -> clientRatelimit(3, "1m", "Authorization")
//    -> "https://login.backend.net";
func NewClientRatelimit(provider RatelimitProvider) filters.Spec {
	return &spec{typ: ratelimit.ClientRatelimit, provider: provider, filterName: ratelimit.ClientRatelimitName}
}

// NewRatelimit creates a service rate limiting, that is
// only aware of itself. If you have 5 instances with 20 req/s, then
// it would at max allow 100 req/s to the backend.
//
// Example:
//
//    backendHealthcheck: Path("/healthcheck")
//    -> ratelimit(20, "1s")
//    -> "https://foo.backend.net";
func NewRatelimit(provider RatelimitProvider) filters.Spec {
	return &spec{typ: ratelimit.ServiceRatelimit, provider: provider, filterName: ratelimit.ServiceRatelimitName}
}

// NewClusterRatelimit creates a rate limiting that is aware of the
// other instances. The value given here should be the combined rate
// of all instances. The ratelimit group parameter can be used to
// select the same ratelimit group across one or more routes.
//
// Example:
//
//    backendHealthcheck: Path("/healthcheck")
//    -> clusterRatelimit("groupA", 200, "1m")
//    -> "https://foo.backend.net";
//
func NewClusterRateLimit(provider RatelimitProvider) filters.Spec {
	return &spec{typ: ratelimit.ClusterServiceRatelimit, provider: provider, filterName: ratelimit.ClusterServiceRatelimitName}
}

// NewClusterClientRatelimit creates a rate limiting that is aware of
// the other instances. The value given here should be the combined
// rate of all instances. The ratelimit group parameter can be used to
// select the same ratelimit group across one or more routes.
//
// Example:
//
//    backendHealthcheck: Path("/login")
//    -> clusterClientRatelimit("groupB", 20, "1h")
//    -> "https://foo.backend.net";
//
// The above example would limit access to "/login" if, the client did
// more than 20 requests within the last hour to this route across all
// running skippers in the cluster.  A single client can be detected
// by different data from the http request and defaults to client IP
// or X-Forwarded-For header, if exists. The optional third parameter
// chooses the HTTP header to choose a client is
// counted as the same.
//
// Example:
//
//    backendHealthcheck: Path("/login")
//    -> clusterClientRatelimit("groupC", 20, "1h", "Authorization")
//    -> "https://foo.backend.net";
//
func NewClusterClientRateLimit(provider RatelimitProvider) filters.Spec {
	return &spec{typ: ratelimit.ClusterClientRatelimit, provider: provider, filterName: ratelimit.ClusterClientRatelimitName}
}

// NewDisableRatelimit disables rate limiting
//
// Example:
//
//    backendHealthcheck: Path("/healthcheck")
//    -> disableRatelimit()
//    -> "https://foo.backend.net";
func NewDisableRatelimit(provider RatelimitProvider) filters.Spec {
	return &spec{typ: ratelimit.DisableRatelimit, provider: provider, filterName: ratelimit.DisableRatelimitName}
}

func (s *spec) Name() string {
	return s.filterName
}

func serviceRatelimitFilter(args []interface{}) (*filter, error) {
	if len(args) != 2 {
		return nil, filters.ErrInvalidFilterParameters
	}

	maxHits, err := getIntArg(args[0])
	if err != nil {
		return nil, err
	}

	timeWindow, err := getDurationArg(args[1])
	if err != nil {
		return nil, err
	}

	return &filter{
		settings: ratelimit.Settings{
			Type:       ratelimit.ServiceRatelimit,
			MaxHits:    maxHits,
			TimeWindow: timeWindow,
			Lookuper:   ratelimit.NewSameBucketLookuper(),
		},
	}, nil
}

func clusterRatelimitFilter(args []interface{}) (*filter, error) {
	if len(args) != 3 {
		return nil, filters.ErrInvalidFilterParameters
	}

	group, err := getStringArg(args[0])
	if err != nil {
		return nil, err
	}

	maxHits, err := getIntArg(args[1])
	if err != nil {
		return nil, err
	}

	timeWindow, err := getDurationArg(args[2])
	if err != nil {
		return nil, err
	}

	s := ratelimit.Settings{
		Type:       ratelimit.ClusterServiceRatelimit,
		Group:      group,
		MaxHits:    maxHits,
		TimeWindow: timeWindow,
		Lookuper:   ratelimit.NewSameBucketLookuper(),
	}

	return &filter{settings: s}, nil
}

func clusterClientRatelimitFilter(args []interface{}) (*filter, error) {
	if !(len(args) == 3 || len(args) == 4) {
		return nil, filters.ErrInvalidFilterParameters
	}

	group, err := getStringArg(args[0])
	if err != nil {
		return nil, err
	}

	maxHits, err := getIntArg(args[1])
	if err != nil {
		return nil, err
	}

	timeWindow, err := getDurationArg(args[2])
	if err != nil {
		return nil, err
	}

	s := ratelimit.Settings{
		Type:          ratelimit.ClusterClientRatelimit,
		Group:         group,
		MaxHits:       maxHits,
		TimeWindow:    timeWindow,
		CleanInterval: 10 * timeWindow,
	}

	if len(args) > 3 {
		lookuperString, err := getStringArg(args[3])
		if err != nil {
			return nil, err
		}
		if strings.Contains(lookuperString, ",") {
			var lookupers []ratelimit.Lookuper
			for _, ls := range strings.Split(lookuperString, ",") {
				lookupers = append(lookupers, getLookuper(ls))
			}
			s.Lookuper = ratelimit.NewTupleLookuper(lookupers...)
		} else {
			s.Lookuper = getLookuper(lookuperString)
		}
	} else {
		s.Lookuper = ratelimit.NewXForwardedForLookuper()
	}

	return &filter{
		settings:   s,
		overwrites: make(map[string]ratelimit.Settings),
	}, nil
}

func getLookuper(s string) ratelimit.Lookuper {
	headerName := http.CanonicalHeaderKey(s)
	if headerName == "X-Forwarded-For" {
		return ratelimit.NewXForwardedForLookuper()
	} else {
		return ratelimit.NewHeaderLookuper(headerName)
	}
}

func clientRatelimitFilter(args []interface{}) (*filter, error) {
	if !(len(args) == 2 || len(args) == 3) {
		return nil, filters.ErrInvalidFilterParameters
	}

	maxHits, err := getIntArg(args[0])
	if err != nil {
		return nil, err
	}

	timeWindow, err := getDurationArg(args[1])
	if err != nil {
		return nil, err
	}

	var lookuper ratelimit.Lookuper
	if len(args) > 2 {
		lookuperString, err := getStringArg(args[2])
		if err != nil {
			return nil, err
		}
		if strings.Contains(lookuperString, ",") {
			var lookupers []ratelimit.Lookuper
			for _, ls := range strings.Split(lookuperString, ",") {
				lookupers = append(lookupers, getLookuper(ls))
			}
			lookuper = ratelimit.NewTupleLookuper(lookupers...)
		} else {
			lookuper = ratelimit.NewHeaderLookuper(lookuperString)
		}
	} else {
		lookuper = ratelimit.NewXForwardedForLookuper()
	}

	return &filter{
		settings: ratelimit.Settings{
			Type:          ratelimit.ClientRatelimit,
			MaxHits:       maxHits,
			TimeWindow:    timeWindow,
			CleanInterval: 10 * timeWindow,
			Lookuper:      lookuper,
		},
	}, nil
}

func disableFilter([]interface{}) (*filter, error) {
	return &filter{
		settings: ratelimit.Settings{
			Type: ratelimit.DisableRatelimit,
		},
	}, nil
}

func (s *spec) CreateFilter(args []interface{}) (filters.Filter, error) {
	f, err := s.createFilter(args)
	if f != nil {
		f.provider = s.provider
	}
	return f, err
}

func (s *spec) createFilter(args []interface{}) (*filter, error) {
	switch s.typ {
	case ratelimit.ServiceRatelimit:
		return serviceRatelimitFilter(args)
	case ratelimit.LocalRatelimit:
		log.Warning("ratelimit.LocalRatelimit is deprecated, please use ratelimit.ClientRatelimit")
		fallthrough
	case ratelimit.ClientRatelimit:
		return clientRatelimitFilter(args)
	case ratelimit.ClusterServiceRatelimit:
		return clusterRatelimitFilter(args)
	case ratelimit.ClusterClientRatelimit:
		return clusterClientRatelimitFilter(args)
	default:
		return disableFilter(args)
	}
}

func getIntArg(a interface{}) (int, error) {
	if i, ok := a.(int); ok {
		return i, nil
	}

	if f, ok := a.(float64); ok {
		return int(f), nil
	}

	return 0, filters.ErrInvalidFilterParameters
}

func getStringArg(a interface{}) (string, error) {
	if s, ok := a.(string); ok {
		return s, nil
	}

	return "", filters.ErrInvalidFilterParameters
}

func getDurationArg(a interface{}) (time.Duration, error) {
	if s, ok := a.(string); ok {
		return time.ParseDuration(s)
	}

	i, err := getIntArg(a)
	return time.Duration(i) * time.Second, err
}

// Request checks ratelimit using filter settings and serves `429 Too Many Requests` response if limit is reached
func (f *filter) Request(ctx filters.FilterContext) {
	rateLimiter := f.provider.get(f.settings)
	if rateLimiter == nil {
		log.Errorf("RateLimiter is nil for settings: %s", f.settings)
		return
	}

	if f.settings.Lookuper == nil {
		log.Errorf("Lookuper is nil for settings: %s", f.settings)
		return
	}

	s := f.settings.Lookuper.Lookup(ctx.Request())
	if s == "" {
		log.Debugf("Lookuper found no data in request for settings: %s and request: %v", f.settings, ctx.Request())
		return
	}

	setting := f.settings
	reqCtx := ctx.Request().Context()

	f.mu.Lock()
	set, ok := f.overwrites[s]
	f.mu.Unlock()
	if ok {
		reqCtx = context.WithValue(reqCtx, ratelimit.RateHeaderOverwrite, set)
		setting = set
	}

	if !rateLimiter.AllowContext(reqCtx, s) {
		ctx.Serve(&http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     ratelimit.Headers(&setting, rateLimiter.RetryAfter(s)),
		})
	}
}

func (f *filter) Response(ctx filters.FilterContext) {
	s := ctx.Response().Header.Get(ratelimit.RateHeaderOverwrite)
	a := strings.Split(s, " ")
	if len(a) != 2 {
		return
	}
	n, err := strconv.Atoi(a[0])
	if err != nil {
		return
	}
	d, err := time.ParseDuration(a[1])
	if err != nil {
		return
	}

	identifyClient := f.settings.Lookuper.Lookup(ctx.Request())
	f.mu.Lock()
	f.overwrites[identifyClient] = ratelimit.Settings{
		Type:          f.settings.Type,
		Group:         f.settings.Group, // TODO(sszuecs): we could change group to merge clients
		MaxHits:       n,
		TimeWindow:    d,
		CleanInterval: 10 * d,
	}
	f.mu.Unlock()

	log.Infof("Added overwrite for %s with %d/%v", identifyClient, n, d)
}
