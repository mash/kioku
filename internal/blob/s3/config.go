package s3

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

type Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	HTTPClient      *http.Client
}

// New configures an S3-compatible store without provider-specific behavior.
func New(c Config) (*Projects, error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("S3_ENDPOINT must be an HTTP(S) origin")
	}
	if strings.TrimSpace(c.Region) == "" || strings.ContainsAny(c.Region, "/\r\n ") || !sharedBucketName.MatchString(c.Bucket) || c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return nil, errors.New("valid S3_REGION, S3_BUCKET, S3_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY are required")
	}
	client := http.Client{Timeout: 60 * time.Second}
	if c.HTTPClient != nil {
		client = *c.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	sdk := awss3.New(awss3.Options{
		Region: c.Region, BaseEndpoint: aws.String(strings.TrimSuffix(c.Endpoint, "/")), UsePathStyle: true,
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, c.SessionToken)),
		HTTPClient:  &client,
		// Optional checksum extensions are not supported by every compatible service.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &Projects{Template: Store{Bucket: c.Bucket, Client: sdk}}, nil
}
