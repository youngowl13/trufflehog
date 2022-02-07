package cloudflarecakey

import (
	"context"
	// "fmt"
	// "log"
	"regexp"
	"strings"

	"net/http"

	"github.com/trufflesecurity/trufflehog/pkg/common"
	"github.com/trufflesecurity/trufflehog/pkg/detectors"
	"github.com/trufflesecurity/trufflehog/pkg/pb/detectorspb"
)

type Scanner struct{}

// Ensure the Scanner satisfies the interface at compile time
var _ detectors.Detector = (*Scanner)(nil)

var (
	client = common.SaneHttpClient()

	keyPat = regexp.MustCompile(detectors.PrefixRegex([]string{"cloudflare"}) + `\b(v[A-Za-z0-9._-]{173,})\b`)
)

// Keywords are used for efficiently pre-filtering chunks.
// Use identifiers in the secret preferably, or the provider name.
func (s Scanner) Keywords() []string {
	return []string{"cloudflare"}
}

// FromData will find and optionally verify CloudflareCaKey secrets in a given set of bytes.
func (s Scanner) FromData(ctx context.Context, verify bool, data []byte) (results []detectors.Result, err error) {
	dataStr := string(data)

	matches := keyPat.FindAllStringSubmatch(dataStr, -1)

	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		resMatch := strings.TrimSpace(match[1])

		if detectors.IsKnownFalsePositive(resMatch, detectors.DefaultFalsePositives, true) {
			continue
		}

		s1 := detectors.Result{
			DetectorType: detectorspb.DetectorType_CloudflareCaKey,
			Raw:          []byte(resMatch),
		}

		if verify {
			req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.cloudflare.com/client/v4/certificates?zone_id=a", nil)
			req.Header.Add("Content-Type", "application/json")
			req.Header.Add("user-agent", "curl/7.68.0") // pretend to be from curl so we do not wait 100+ seconds -> nice try did not work

			req.Header.Add("X-Auth-User-Service-Key", resMatch)
			res, err := client.Do(req)
			if err == nil {
				defer res.Body.Close()
				if res.StatusCode >= 200 && res.StatusCode < 300 {
					s1.Verified = true
				}
			} else {
			}
		}

		results = append(results, s1)
	}

	return detectors.CleanResults(results), nil
}
