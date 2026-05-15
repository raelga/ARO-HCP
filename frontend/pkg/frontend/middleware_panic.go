// Copyright 2025 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package frontend

import (
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"k8s.io/component-base/metrics/legacyregistry"

	"github.com/Azure/ARO-HCP/internal/api/arm"
	"github.com/Azure/ARO-HCP/internal/utils"
)

var (
	// httpRequestsPanicTotalCounter counts the total number of panics that occurred
	// during request handling.
	// To get the total panics use sum() in PromQL for the aggregate.
	// The label values must never be empty.
	httpRequestPanicsTotalCounter = promauto.With(legacyregistry.Registerer()).NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_request_panics_total",
			Help: "Total number of panics during HTTP request handling. Labels: method (HTTP method, 'unknown' if empty), path_pattern (route pattern without method prefix, 'unmatched' if no route matched).",
		},
		[]string{"path_pattern", "method"},
	)
)

func MiddlewarePanic(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	// Do not catch panics when running "go test".
	if !testing.Testing() {
		defer func() {
			if e := recover(); e != nil {
				logger := utils.LoggerFromContext(r.Context())
				panicErr := fmt.Errorf("panic: %#v", e)
				logger.Error(panicErr, "panic recovered", "stack", string(debug.Stack()))

				// r.Pattern can be empty if the request didn't match against any pattern
				// and this middleware is executed on that request-response flow. In
				// that case we set a sentinel value of "unmatched" for the path_pattern
				// prometheus label
				requestPattern := r.Pattern
				if requestPattern == "" {
					requestPattern = "unmatched"
				}
				// We make sure we strip the method part of r.Pattern in requestPattern
				// in the case the pattern includes the method initially. This is because
				// for the prometheus counter we store the request path and the
				// request method separetely so we can filter them independently.
				if i := strings.IndexAny(requestPattern, " \t"); i != -1 {
					requestPattern = strings.TrimLeft(requestPattern[i+1:], " \t")
				}
				requestMethod := r.Method
				if requestMethod == "" {
					requestMethod = "unknown"
				}
				httpRequestPanicsTotalCounter.WithLabelValues(requestPattern, requestMethod).Inc()

				arm.WriteInternalServerError(w)
			}
		}()
	}

	next(w, r)
}
