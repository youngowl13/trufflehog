package sumologickey

import (
	"context"
	"fmt"

	// "log"
	b64 "encoding/base64"
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

	//Make sure that your group is surrounded in boundry characters such as below to reduce false positives
	idPat  = regexp.MustCompile(detectors.PrefixRegex([]string{"sumo"}) + `\b([A-Za-z0-9]{14})\b`)
	keyPat = regexp.MustCompile(detectors.PrefixRegex([]string{"sumo"}) + `\b([A-Za-z0-9]{64})\b`)
)

// Keywords are used for efficiently pre-filtering chunks.
// Use identifiers in the secret preferably, or the provider name.
func (s Scanner) Keywords() []string {
	return []string{"sumologic"}
}

// FromData will find and optionally verify SumoLogicKey secrets in a given set of bytes.
func (s Scanner) FromData(ctx context.Context, verify bool, data []byte) (results []detectors.Result, err error) {
	dataStr := string(data)
	idMatches := idPat.FindAllStringSubmatch(dataStr, -1)
	matches := keyPat.FindAllStringSubmatch(dataStr, -1)

	for _, idMatch := range idMatches {
		if len(idMatch) != 2 {
			continue
		}
		resIdMatch := strings.TrimSpace(idMatch[1])
		for _, match := range matches {
			if len(match) != 2 {
				continue
			}
			resMatch := strings.TrimSpace(match[1])

			s1 := detectors.Result{
				DetectorType: detectorspb.DetectorType_SumoLogicKey,
				Raw:          []byte(resMatch),
			}

			if verify {
				data := fmt.Sprintf("%s:%s", resIdMatch, resMatch)
				encoded := b64.StdEncoding.EncodeToString([]byte(data))
				req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.us2.sumologic.com/api/v1/users", nil)
				req.Header.Add("Authorization", fmt.Sprintf("Basic %s", encoded))
				res, err := client.Do(req)
				if err == nil {
					defer res.Body.Close()
					if res.StatusCode >= 200 && res.StatusCode < 300 {
						s1.Verified = true
					} else {
						//This function will check false positives for common test words, but also it will make sure the key appears 'random' enough to be a real key
						if detectors.IsKnownFalsePositive(resMatch, detectors.DefaultFalsePositives, true) {
							continue
						}
					}
				}
			}

			results = append(results, s1)
		}

	}

	return detectors.CleanResults(results), nil
}
