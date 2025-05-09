// Package middleware - cloudfront wrapper for storage libs
// N.B. currently only works with S3, not arbitrary sites
package middleware

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/service/cloudfront/sign"
	"github.com/distribution/distribution/v3/internal/dcontext"
	storagedriver "github.com/distribution/distribution/v3/registry/storage/driver"
	storagemiddleware "github.com/distribution/distribution/v3/registry/storage/driver/middleware"
	"github.com/sirupsen/logrus"
)

// init registers the cloudfront layerHandler backend.
func init() {
	if err := storagemiddleware.Register("cloudfront", newCloudFrontStorageMiddleware); err != nil {
		logrus.Errorf("failed to register cloudfront middleware: %v", err)
	}
}

// cloudFrontStorageMiddleware provides a simple implementation of layerHandler that
// constructs temporary signed CloudFront URLs from the storagedriver layer URL,
// then issues HTTP Temporary Redirects to this CloudFront content URL.
type cloudFrontStorageMiddleware struct {
	storagedriver.StorageDriver
	awsIPs    *awsIPs
	urlSigner *sign.URLSigner
	baseURL   string
	duration  time.Duration
}

var _ storagedriver.StorageDriver = &cloudFrontStorageMiddleware{}

// newCloudFrontStorageMiddleware constructs and returns a new CloudFront
// LayerHandler implementation.
//
// Required options:
//
//   - baseurl
//   - privatekey
//   - keypairid
//
// Optional options:
//
//   - ipFilteredBy
//   - awsregion
//   - ipfilteredby: valid value "none|aws|awsregion". "none", do not filter any IP,
//     default value. "aws", only aws IP goes to S3 directly. "awsregion", only
//     regions listed in awsregion options goes to S3 directly
//   - awsregion: a comma separated string of AWS regions.
func newCloudFrontStorageMiddleware(ctx context.Context, storageDriver storagedriver.StorageDriver, options map[string]interface{}) (storagedriver.StorageDriver, error) {
	// parse baseurl
	base, ok := options["baseurl"]
	if !ok {
		return nil, fmt.Errorf("no baseurl provided")
	}
	baseURL, ok := base.(string)
	if !ok {
		return nil, fmt.Errorf("baseurl must be a string")
	}
	if !strings.Contains(baseURL, "://") {
		baseURL = "https://" + baseURL
	}
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	if _, err := url.Parse(baseURL); err != nil {
		return nil, fmt.Errorf("invalid baseurl: %v", err)
	}

	// parse privatekey to get pkPath
	pk, ok := options["privatekey"]
	if !ok {
		return nil, fmt.Errorf("no privatekey provided")
	}
	pkPath, ok := pk.(string)
	if !ok {
		return nil, fmt.Errorf("privatekey must be a string")
	}

	// parse keypairid
	kpid, ok := options["keypairid"]
	if !ok {
		return nil, fmt.Errorf("no keypairid provided")
	}
	keypairID, ok := kpid.(string)
	if !ok {
		return nil, fmt.Errorf("keypairid must be a string")
	}

	// get urlSigner from the file specified in pkPath
	pkBytes, err := os.ReadFile(pkPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read privatekey file: %s", err)
	}

	block, _ := pem.Decode(pkBytes)
	if block == nil {
		return nil, fmt.Errorf("failed to decode private key as an rsa private key")
	}
	privateKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	urlSigner := sign.NewURLSigner(keypairID, privateKey)

	// parse duration
	duration := 20 * time.Minute
	if d, ok := options["duration"]; ok {
		switch d := d.(type) {
		case time.Duration:
			duration = d
		case string:
			dur, err := time.ParseDuration(d)
			if err != nil {
				return nil, fmt.Errorf("invalid duration: %s", err)
			}
			duration = dur
		}
	}

	// parse updatefrequency
	updateFrequency := defaultUpdateFrequency
	// #2447 introduced a typo. Support it for backward compatibility.
	if _, ok := options["updatefrenquency"]; ok {
		options["updatefrequency"] = options["updatefrenquency"]
		dcontext.GetLogger(context.Background()).Warn("cloudfront updatefrenquency is deprecated. Please use updatefrequency")
	}
	if u, ok := options["updatefrequency"]; ok {
		switch u := u.(type) {
		case time.Duration:
			updateFrequency = u
		case string:
			updateFreq, err := time.ParseDuration(u)
			if err != nil {
				return nil, fmt.Errorf("invalid updatefrequency: %s", err)
			}
			updateFrequency = updateFreq
		}
	}

	// parse iprangesurl
	ipRangesURL := defaultIPRangesURL
	if i, ok := options["iprangesurl"]; ok {
		if iprangeurl, ok := i.(string); ok {
			ipRangesURL = iprangeurl
		} else {
			return nil, fmt.Errorf("iprangesurl must be a string")
		}
	}

	// parse ipfilteredby
	var awsIPs *awsIPs
	if i, ok := options["ipfilteredby"]; ok {
		if ipFilteredBy, ok := i.(string); ok {
			switch strings.ToLower(strings.TrimSpace(ipFilteredBy)) {
			case "", "none":
				awsIPs = nil
			case "aws":
				awsIPs, err = newAWSIPs(ctx, ipRangesURL, updateFrequency, nil)
				if err != nil {
					return nil, err
				}
			case "awsregion":
				var awsRegion []string
				if i, ok := options["awsregion"]; ok {
					if regions, ok := i.(string); ok {
						for _, awsRegions := range strings.Split(regions, ",") {
							awsRegion = append(awsRegion, strings.ToLower(strings.TrimSpace(awsRegions)))
						}
						awsIPs, err = newAWSIPs(ctx, ipRangesURL, updateFrequency, awsRegion)
						if err != nil {
							return nil, err
						}
					} else {
						return nil, fmt.Errorf("awsRegion must be a comma separated string of valid aws regions")
					}
				} else {
					return nil, fmt.Errorf("awsRegion is not defined")
				}
			default:
				return nil, fmt.Errorf("ipfilteredby only allows a string the following value: none|aws|awsregion")
			}
		} else {
			return nil, fmt.Errorf("ipfilteredby only allows a string with the following value: none|aws|awsregion")
		}
	}

	return &cloudFrontStorageMiddleware{
		StorageDriver: storageDriver,
		urlSigner:     urlSigner,
		baseURL:       baseURL,
		duration:      duration,
		awsIPs:        awsIPs,
	}, nil
}

// S3BucketKeyer is any type that is capable of returning the S3 bucket key
// which should be cached by AWS CloudFront.
type S3BucketKeyer interface {
	S3BucketKey(path string) string
}

// RedirectURL attempts to find a url which may be used to retrieve the file at the given path.
// Returns an error if the file cannot be found.
func (lh *cloudFrontStorageMiddleware) RedirectURL(r *http.Request, path string) (string, error) {
	// TODO(endophage): currently only supports S3
	keyer, ok := lh.StorageDriver.(S3BucketKeyer)
	if !ok {
		dcontext.GetLogger(r.Context()).Warn("the CloudFront middleware does not support this backend storage driver")
		return lh.StorageDriver.RedirectURL(r, path)
	}

	if eligibleForS3(r, lh.awsIPs) {
		return lh.StorageDriver.RedirectURL(r, path)
	}

	// Get signed cloudfront url.
	cfURL, err := lh.urlSigner.Sign(lh.baseURL+keyer.S3BucketKey(path), time.Now().Add(lh.duration))
	if err != nil {
		return "", err
	}
	return cfURL, nil
}
