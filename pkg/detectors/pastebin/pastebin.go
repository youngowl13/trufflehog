package pastebin

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"

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

	//Make sure that your group is surrounded in boundry characters such as below to reduce false positives
	keyPat = regexp.MustCompile(detectors.PrefixRegex([]string{"pastebin"}) + `\b([a-zA-Z0-9_]{32})\b`)
)

// Keywords are used for efficiently pre-filtering chunks.
// Use identifiers in the secret preferably, or the provider name.
func (s Scanner) Keywords() []string {
	return []string{"pastebin"}
}

// FromData will find and optionally verify Pastebin secrets in a given set of bytes.
func (s Scanner) FromData(ctx context.Context, verify bool, data []byte) (results []detectors.Result, err error) {
	dataStr := string(data)

	matches := keyPat.FindAllStringSubmatch(dataStr, -1)

	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		resMatch := strings.TrimSpace(match[1])

		s1 := detectors.Result{
			DetectorType: detectorspb.DetectorType_Pastebin,
			Raw:          []byte(resMatch),
		}

		if verify {
			body := &bytes.Buffer{}
			writer := multipart.NewWriter(body)
			fw, err := writer.CreateFormField("api_dev_key")
			if err != nil {
				continue
			}
			_, err = io.Copy(fw, strings.NewReader(resMatch))
			if err != nil {
				continue
			}
			fw, err = writer.CreateFormField("api_paste_code")
			if err != nil {
				continue
			}
			_, err = io.Copy(fw, strings.NewReader("test"))
			if err != nil {
				continue
			}
			fw, err = writer.CreateFormField("api_option")
			if err != nil {
				continue
			}
			_, err = io.Copy(fw, strings.NewReader("paste"))
			if err != nil {
				continue
			}
			writer.Close()
			req, _ := http.NewRequestWithContext(ctx, "POST", "https://pastebin.com/api/api_post.php", bytes.NewReader(body.Bytes()))
			req.Header.Add("Content-Type", writer.FormDataContentType())
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

	return detectors.CleanResults(results), nil
}
