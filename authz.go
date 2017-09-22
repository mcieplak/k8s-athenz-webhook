package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	authz "k8s.io/api/authorization/v1beta1"
)

const (
	authzSupportedVersion = "authorization.k8s.io/v1beta1"
	authzSupportedKind    = "SubjectAccessReview"
)

// AuthzError is an error implementation that can provide custom
// messages for the reason field in the
// SubjectAccessReviewStatus object.
type AuthzError struct {
	error
	reason string
}

// NewAuthzError returns an error implementation whose reason
// member is copied into the returned status object.
func NewAuthzError(delegate error, reason string) *AuthzError {
	return &AuthzError{
		error:  delegate,
		reason: reason,
	}
}

// Reason returns the string that should be copied into the `reason`
// field of the status object.
func (a *AuthzError) Reason() string {
	return a.reason
}

type authorizer struct {
	AuthorizationConfig
}

func newAuthz(c AuthorizationConfig) *authorizer {
	return &authorizer{
		AuthorizationConfig: c,
	}
}

func (a *authorizer) client(ctx context.Context) (*client, error) {
	tok, err := a.Token()
	if err != nil {
		return nil, err
	}
	xp := newAuthTransport(a.AuthHeader, tok)
	if isLogEnabled(ctx, LogTraceAthenz) {
		xp = &debugTransport{
			log:          getLogger(ctx),
			RoundTripper: xp,
		}
	}
	return newClient(a.Endpoint, a.Timeout, xp), nil
}

// getSubjectAccessReview extracts the subject access review object from the request and returns it.
func (a *authorizer) getSubjectAccessReview(ctx context.Context, req *http.Request) (*authz.SubjectAccessReview, error) {
	b, err := ioutil.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("body read error for authorization request, %v", err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("empty body for authorization request")
	}
	if isLogEnabled(ctx, LogTraceServer) {
		getLogger(ctx).Printf("request body: %s\n", b)
	}
	var r authz.SubjectAccessReview
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("invalid JSON request '%s', %v", b, err)
	}
	if r.APIVersion != authzSupportedVersion {
		return nil, fmt.Errorf("unsupported authorization version, want '%s', got '%s'", authzSupportedVersion, r.APIVersion)
	}
	if r.Kind != authzSupportedKind {
		return nil, fmt.Errorf("unsupported authorization kind, want '%s', got '%s'", authzSupportedKind, r.Kind)
	}
	if r.Spec.ResourceAttributes == nil && r.Spec.NonResourceAttributes == nil {
		return nil, fmt.Errorf("bad authorization spec, must have one of resource or non-resource attributes")
	}
	return &r, nil
}

// grantStatus adds extra information to a review status.
type grantStatus struct {
	status authz.SubjectAccessReviewStatus // the status to be returned to the client
	via    string                          // the resource check that succeeded for a grant, not set for deny
}

func (a *authorizer) authorize(ctx context.Context, sr authz.SubjectAccessReviewSpec) *grantStatus {
	deny := func(err error) *grantStatus {
		var reason string
		if e, ok := err.(*AuthzError); ok {
			reason = e.Reason()
		}
		reason += a.HelpMessage
		return &grantStatus{
			status: authz.SubjectAccessReviewStatus{
				Allowed:         false,
				Reason:          reason,
				EvaluationError: err.Error(),
			},
		}
	}
	principal, checks, err := a.Mapper.MapResource(ctx, sr)
	if err != nil {
		return deny(fmt.Errorf("mapping error: %v", err))
	}
	var granted bool
	if len(checks) == 0 { // grant it by API contract
		return &grantStatus{
			status: authz.SubjectAccessReviewStatus{
				Allowed: true,
			},
			via: "no Athenz resource checks needed",
		}
	}
	internal := "internal setup error."
	var via string
	for _, check := range checks {
		client, err := a.client(ctx)
		if err != nil {
			return deny(NewAuthzError(err, internal))
		}
		granted, err = client.authorize(principal, check)
		if err != nil {
			switch e := err.(type) {
			case *statusCodeError:
				switch e.code {
				case http.StatusUnauthorized: // identity token was borked
					return deny(NewAuthzError(err, internal))
				case http.StatusNotFound: // domain setup error
					return deny(NewAuthzError(fmt.Errorf("domain related error for %v, %v", check, err), fmt.Sprintf("Athenz domain error.")))
				}
			}
			return deny(NewAuthzError(err, ""))
		}
		if granted {
			via = check.String()
			break
		}
	}
	if !granted {
		var list []string
		for _, c := range checks {
			list = append(list, fmt.Sprintf("'%s'", c))
		}
		msg := fmt.Sprintf("principal %s does not have access to any of %s resources", principal, strings.Join(list, ","))
		return deny(errors.New(msg)) // not showing this msg to the user, should we?
	}
	return &grantStatus{
		status: authz.SubjectAccessReviewStatus{
			Allowed: true,
		},
		via: via,
	}
}

func (a *authorizer) logOutcome(logger Logger, sr *authz.SubjectAccessReviewSpec, gs *grantStatus) {
	srText := "unknown"
	switch {
	case sr.ResourceAttributes != nil:
		ra := sr.ResourceAttributes
		srText = fmt.Sprintf("%s: %s on %s:%s:%s:%s", sr.User, ra.Verb, ra.Namespace, ra.Resource, ra.Subresource, ra.Name)
	case sr.NonResourceAttributes != nil:
		nra := sr.NonResourceAttributes
		srText = fmt.Sprintf("%s: %s on %s", sr.User, nra.Verb, nra.Path)
	}

	var srDebug string
	b, err := json.Marshal(sr)
	if err == nil {
		srDebug = " (" + string(b) + ")"
	}
	granted := "granted"
	status := gs.status
	if !status.Allowed {
		granted = "denied"
	}
	var add string
	if gs.via != "" {
		add += "via " + gs.via
	}
	if status.EvaluationError != "" {
		add += "error:" + status.EvaluationError
		if status.Reason != "" {
			add += ", reason:" + status.Reason
		}
	}
	logger.Printf("authz %s %s -> %s%s\n", granted, srText, add, srDebug)
}

func (a *authorizer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sr, err := a.getSubjectAccessReview(ctx, r)
	if err != nil {
		getLogger(ctx).Printf("authz request error from %s: %v\n", r.RemoteAddr, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	gs := a.authorize(ctx, sr.Spec)
	a.logOutcome(getLogger(ctx), &sr.Spec, gs)

	resp := struct {
		APIVersion string                          `json:"apiVersion"`
		Kind       string                          `json:"kind"`
		Status     authz.SubjectAccessReviewStatus `json:"status"`
	}{sr.APIVersion, sr.Kind, gs.status}
	writeJSON(ctx, w, &resp)
}