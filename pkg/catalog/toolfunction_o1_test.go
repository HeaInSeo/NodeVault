package catalog_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/index"
	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

// O-1 authoring binding bridge (CLOSED MINIMUM): whole-element {param|input|output.<name>}
// references in CommandContract.arguments, name grammar [A-Za-z][A-Za-z0-9._-]*, exact-one
// resolution, malformed/embedded rejection, repeated references allowed, consume-all, and
// direct {input.*}/{output.*} not runnable before a B profile. Validation runs before digest
// and before any persistent mutation.

func o1Req(args []string, params ...string) *nfv1.RegisterToolFunctionRequest {
	req := validTFReq()
	req.Spec.Command.Arguments = args
	req.Spec.Parameters = nil
	for _, name := range params {
		req.Spec.Parameters = append(req.Spec.Parameters,
			&nfv1.ParameterSpec{Name: name, Type: nfv1.ParameterType_PARAMETER_TYPE_STRING})
	}
	return req
}

func TestRegisterToolFunction_O1ValidReferences(t *testing.T) {
	cases := []struct {
		name string
		req  func() *nfv1.RegisterToolFunctionRequest
	}{
		{"zero args zero params", func() *nfv1.RegisterToolFunctionRequest { return o1Req(nil) }},
		{"literal-only args", func() *nfv1.RegisterToolFunctionRequest { return o1Req([]string{"sort", "-@4"}) }},
		{"single ref", func() *nfv1.RegisterToolFunctionRequest {
			return o1Req([]string{"sort", "{param.threads}"}, "threads")
		}},
		{"repeated ref", func() *nfv1.RegisterToolFunctionRequest {
			return o1Req([]string{"{param.t}", "x", "{param.t}"}, "t")
		}},
		{"grammar edge", func() *nfv1.RegisterToolFunctionRequest {
			return o1Req([]string{"{param.a.b-c_1}"}, "a.b-c_1")
		}},
		{"case-sensitive distinct", func() *nfv1.RegisterToolFunctionRequest {
			return o1Req([]string{"{param.t}", "{param.T}"}, "t", "T")
		}},
		{"boolean with mapping", func() *nfv1.RegisterToolFunctionRequest {
			r := o1Req([]string{"{param.v}"})
			r.Spec.Parameters = []*nfv1.ParameterSpec{{
				Name: "v", Type: nfv1.ParameterType_PARAMETER_TYPE_BOOLEAN, CliArgumentMapping: "--verbose",
			}}
			return r
		}},
		// Undecided reference-like boundary: these stay literal (same as the NodeKit client gate).
		{"open boundary literals", func() *nfv1.RegisterToolFunctionRequest {
			return o1Req([]string{"{}", "{foo}", "{1.5}", "{ param.t }", "{param.t}"}, "t")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newTFService(t)
			mustRegisterTF(t, svc, tc.req())
		})
	}
}

func TestRegisterToolFunction_O1InvalidReferencesZeroMutation(t *testing.T) {
	cases := []struct {
		name   string
		req    *nfv1.RegisterToolFunctionRequest
		substr string
	}{
		{"unresolved param", o1Req([]string{"{param.nope}"}), "undeclared parameter"},
		{"embedded flag=ref", o1Req([]string{"--t={param.threads}", "{param.threads}"}, "threads"), "malformed or embedded"},
		{"embedded prefix", o1Req([]string{"x{param.t}", "{param.t}"}, "t"), "malformed or embedded"},
		{"embedded suffix", o1Req([]string{"{param.t}x"}, "t"), "malformed or embedded"},
		{"double brace", o1Req([]string{"{{param.t}}", "{param.t}"}, "t"), "malformed or embedded"},
		{"empty target", o1Req([]string{"{param.}"}), "malformed or embedded"},
		{"digit-leading name", o1Req([]string{"{param.1bad}"}), "malformed or embedded"},
		{"space in name", o1Req([]string{"{param.a b}"}), "malformed or embedded"},
		{"underscore-leading name", o1Req([]string{"{param._x}"}), "malformed or embedded"},
		{"non-ASCII name", o1Req([]string{"{param.é}"}), "malformed or embedded"},
		{"wrong namespace", o1Req([]string{"{foo.x}"}), "malformed or embedded"},
		{"plural namespace", o1Req([]string{"{params.x}"}), "malformed or embedded"},
		{"namespace case", o1Req([]string{"{Param.x}"}), "malformed or embedded"},
		{"namespace with digit", o1Req([]string{"{params2.x}"}), "malformed or embedded"},
		{"param ref to input port", o1Req([]string{"{param.reads}"}), "undeclared parameter"},
		{"input ref not runnable", o1Req([]string{"{input.reads}"}), "not runnable"},
		{"output ref not runnable", o1Req([]string{"{output.aligned}"}), "not runnable"},
		{"input ref to parameter name", o1Req([]string{"{input.threads}", "{param.threads}"}, "threads"), "not runnable"},
		{"param not consumed", o1Req([]string{"mem", "-t"}, "threads"), "not referenced"},
		{"mapping does not consume", func() *nfv1.RegisterToolFunctionRequest {
			r := o1Req([]string{"mem"})
			r.Spec.Parameters = []*nfv1.ParameterSpec{{
				Name: "threads", Type: nfv1.ParameterType_PARAMETER_TYPE_INTEGER, CliArgumentMapping: "-t",
				DefaultValue: "4", Required: true,
			}}
			return r
		}(), "not referenced"},
		{"unreferenceable declaration", o1Req([]string{"mem"}, "1bad"), "not referenced"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, store := newTFService(t)
			_, err := svc.RegisterToolFunction(context.Background(), tc.req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("want InvalidArgument, got %v", err)
			}
			if !strings.Contains(status.Convert(err).Message(), tc.substr) {
				t.Fatalf("error %q does not contain %q", status.Convert(err).Message(), tc.substr)
			}
			if _, gerr := store.GetToolFunctionRequestRecord(tc.req.GetRequestId()); !errors.Is(gerr, index.ErrNotFound) {
				t.Fatalf("rejected registration must not persist a request record; got %v", gerr)
			}
		})
	}
}

// TestRegisterToolFunction_O1ValidationOrdering proves the O-1 check runs before the base
// lookup and before the request_id idempotency/conflict checks: an invalid spec is
// InvalidArgument even when the base is missing or the request_id was already used, and the
// stored record is left unchanged.
func TestRegisterToolFunction_O1ValidationOrdering(t *testing.T) {
	t.Run("before base lookup", func(t *testing.T) {
		svc, _ := newTFService(t)
		req := o1Req([]string{"mem"}, "threads")
		req.BaseToolSpecDigest = strings.Repeat("ef", 32) // not seeded → NotFound if reached
		if _, err := svc.RegisterToolFunction(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("want InvalidArgument before base lookup, got %v", err)
		}
	})
	t.Run("replay with same request_id", func(t *testing.T) {
		svc, store := newTFService(t)
		first := mustRegisterTF(t, svc, validTFReq())
		before, err := store.GetToolFunctionRequestRecord("req-1")
		if err != nil {
			t.Fatalf("request record: %v", err)
		}
		replay := validTFReq()
		replay.Spec.Command.Arguments = []string{"mem", "-t", "--t={param.threads}"}
		if _, err := svc.RegisterToolFunction(context.Background(), replay); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("want InvalidArgument for invalid replay (not AlreadyExists), got %v", err)
		}
		after, err := store.GetToolFunctionRequestRecord("req-1")
		if err != nil {
			t.Fatalf("request record after replay: %v", err)
		}
		if after != before {
			t.Fatalf("invalid replay mutated the stored request record: %+v -> %+v", before, after)
		}
		if again := mustRegisterTF(t, svc, validTFReq()); again.GetCasHash() != first.GetCasHash() {
			t.Fatal("valid replay must still converge to the original identity")
		}
	})
}
