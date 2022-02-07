package contentfulpersonalaccesstoken

import (
	"context"
	"fmt"

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
	keyPat = regexp.MustCompile(`\b([CFPAT\-a-zA-Z-0-9]{49})\b`)
)

// Keywords are used for efficiently pre-filtering chunks.
// Use identifiers in the secret preferably, or the provider name.
func (s Scanner) Keywords() []string {
	return []string{"CFPAT-"}
}

// FromData will find and optionally verify ContentfulDelivery secrets in a given set of bytes.
func (s Scanner) FromData(ctx context.Context, verify bool, data []byte) (results []detectors.Result, err error) {
	dataStr := string(data)

	keyMatches := keyPat.FindAllStringSubmatch(dataStr, -1)

	for _, match := range keyMatches {
		if len(match) != 2 {
			continue
		}
		keyRes := strings.TrimSpace(match[1])

		s1 := detectors.Result{
			DetectorType: detectorspb.DetectorType_ContentfulPersonalAccessToken,
			Raw:          []byte(keyRes),
		}

		if verify {
			req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.contentful.com/organizations", nil)
			req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", keyRes))
			res, err := client.Do(req)
			if err == nil {
				defer res.Body.Close()
				if res.StatusCode >= 200 && res.StatusCode < 300 {
					s1.Verified = true
				} else {
					if detectors.IsKnownFalsePositive(keyRes, detectors.DefaultFalsePositives, true) {
						continue
					}
				}
			}
		}

		results = append(results, s1)

	}

	return detectors.CleanResults(results), nil
}
