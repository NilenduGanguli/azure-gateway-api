package surface

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
)

// checkDIParams applies the sync-only spec's parameter policy (§2) to both DI analyze routes.
//
// Every analysis now goes through the container's :syncAnalyze, which declares only api-version,
// pages, locale and stringIndexType. ASP.NET silently ignores a query parameter an action does not
// bind, so forwarding the four :analyze-only ones would hand a client a result quietly lacking what
// it asked for. They are refused until the probe shows the sync route honours them; output=pdf and
// output=figures are refused for good, because those files are fetched by an operation id that a
// synchronous call never creates. Anything else, unknown parameters included, is forwarded.
//
// Names are matched case-insensitively, as ASP.NET binds them: Features=… reaches the container's
// features binding just as features=… does.
func checkDIParams(q url.Values) *azerr.APIError {
	values := func(name string) (vs []string, present bool) {
		for k, v := range q {
			if strings.EqualFold(k, name) {
				vs, present = append(vs, v...), true
			}
		}
		return vs, present
	}

	outputs, _ := values("output")
	for _, v := range outputs {
		for _, o := range strings.Split(v, ",") {
			switch strings.ToLower(strings.TrimSpace(o)) {
			case "pdf", "figures":
				return invalidParam("output=pdf and output=figures are not supported by this gateway.")
			}
		}
	}
	formats, _ := values("outputContentFormat")
	for _, v := range formats {
		if v != "" && !strings.EqualFold(v, "text") {
			return invalidParam("outputContentFormat=" + v + " is not supported by this gateway yet.")
		}
	}
	features, _ := values("features")
	for _, v := range features {
		if v != "" {
			return invalidParam("features is not supported by this gateway yet.")
		}
	}
	if _, present := values("queryFields"); present {
		return invalidParam("queryFields is not supported by this gateway yet.")
	}
	return nil
}

func invalidParam(message string) *azerr.APIError {
	return azerr.New(http.StatusBadRequest, azerr.CodeInvalidArgument, "").
		WithInner(azerr.InnerInvalidParameter, message)
}
