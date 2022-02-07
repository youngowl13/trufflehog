package mapbox

import (
	"context"
	// "log"
	"net/http"
	"regexp"
	"strings"

	"github.com/trufflesecurity/trufflehog/pkg/common"
	"github.com/trufflesecurity/trufflehog/pkg/detectors"
	"github.com/trufflesecurity/trufflehog/pkg/pb/detectorspb"
)

type Scanner struct{}

// Ensure the Scanner satisfies the interface at compile time
var _ detectors.Detector = (*Scanner)(nil)

var (
	client = common.SaneHttpClient()
	idPat  = regexp.MustCompile(`([a-zA-Z-0-9]{4,32})`)
	keyPat = regexp.MustCompile(`\b(sk\.[a-zA-Z-0-9\.]{80,240})\b`)
)

// Keywords are used for efficiently pre-filtering chunks.
// Use identifiers in the secret preferably, or the provider name.
func (s Scanner) Keywords() []string {
	return []string{"mapbox"}
}

// FromData will find and optionally verify MapBox secrets in a given set of bytes.
func (s Scanner) FromData(ctx context.Context, verify bool, data []byte) (results []detectors.Result, err error) {
	dataStr := string(data)

	matches := keyPat.FindAllStringSubmatch(dataStr, -1)
	idMatches := idPat.FindAllStringSubmatch(dataStr, -1)
	for _, match := range matches {

		resMatch := strings.TrimSpace(match[1])
		for i, idMatch := range idMatches {
			if i == 11 {
				if len(idMatch) != 2 {
					continue
				}
				resId := strings.TrimSpace(idMatch[1])

				s1 := detectors.Result{
					DetectorType: detectorspb.DetectorType_MapBox,
					Raw:          []byte(resMatch),
				}

				if verify {
					req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.mapbox.com/tokens/v2/"+resId+"?access_token="+resMatch, nil)
					res, err := client.Do(req)
					if err == nil {
						defer res.Body.Close()
						if res.StatusCode >= 200 && res.StatusCode < 300 {
							s1.Verified = true
						} else {
							if detectors.IsKnownFalsePositive(resMatch, detectors.DefaultFalsePositives, true) {
								continue
							}
						}
					}
				}
				results = append(results, s1)
			}

		}

	}

	return detectors.CleanResults(results), nil
}
