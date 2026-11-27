package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
)

// OSSConfig 是阿里云 OSS 的连接参数。
type OSSConfig struct {
	Region          string `json:"region"`   // 如 cn-hangzhou
	Endpoint        string `json:"endpoint"` // 可选，默认 oss-<地域>.aliyuncs.com
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"access_key_id"`
	AccessKeySecret string `json:"access_key_secret"`
	Prefix          string `json:"prefix"` // 对象路径前缀，如 banbu/；为空时放在桶根目录
}

// Validate 检查必填项并规范前缀。
func (c *OSSConfig) Validate() error {
	c.Region = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(c.Region), "oss-"))
	c.Endpoint = strings.TrimSpace(c.Endpoint)
	c.Bucket = strings.TrimSpace(c.Bucket)
	c.AccessKeyID = strings.TrimSpace(c.AccessKeyID)
	c.AccessKeySecret = strings.TrimSpace(c.AccessKeySecret)
	c.Prefix = strings.Trim(strings.TrimSpace(c.Prefix), "/")
	if c.Prefix != "" {
		if _, err := CleanKey(c.Prefix); err != nil {
			return err
		}
		c.Prefix += "/"
	}
	switch {
	case c.Region == "":
		return errors.New("请填写地域，如 cn-hangzhou")
	case c.Bucket == "":
		return errors.New("请填写 Bucket")
	case c.AccessKeyID == "" || c.AccessKeySecret == "":
		return errors.New("请填写 AccessKey ID 和 AccessKey Secret")
	}
	return nil
}

// OSS 是阿里云 OSS 存储（V4 签名）。
type OSS struct {
	cfg    OSSConfig
	client *oss.Client
}

// NewOSS 按配置创建 OSS 存储，不发请求。
func NewOSS(cfg OSSConfig) (*OSS, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	c := oss.LoadDefaultConfig().
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.AccessKeySecret)).
		WithRegion(cfg.Region).
		WithConnectTimeout(10 * time.Second)
	if cfg.Endpoint != "" {
		c = c.WithEndpoint(cfg.Endpoint)
	}
	return &OSS{cfg: cfg, client: oss.NewClient(c)}, nil
}

func (o *OSS) Kind() string { return KindOSS }

// Key 加上配置的前缀。记录里保存的是加了前缀的完整路径，改前缀不影响已有文件。
func (o *OSS) Key(key string) string { return o.cfg.Prefix + key }

func (o *OSS) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	req := &oss.PutObjectRequest{Bucket: &o.cfg.Bucket, Key: &key, Body: r}
	if size >= 0 {
		req.ContentLength = &size
	}
	_, err := o.client.PutObject(ctx, req)
	return wrap(err)
}

func (o *OSS) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	res, err := o.client.GetObject(ctx, &oss.GetObjectRequest{Bucket: &o.cfg.Bucket, Key: &key})
	if err != nil {
		return nil, 0, wrap(err)
	}
	return res.Body, res.ContentLength, nil
}

func (o *OSS) Stat(ctx context.Context, key string) (int64, error) {
	res, err := o.client.HeadObject(ctx, &oss.HeadObjectRequest{Bucket: &o.cfg.Bucket, Key: &key})
	if err != nil {
		return 0, wrap(err)
	}
	return res.ContentLength, nil
}

func (o *OSS) Delete(ctx context.Context, key string) error {
	_, err := o.client.DeleteObject(ctx, &oss.DeleteObjectRequest{Bucket: &o.cfg.Bucket, Key: &key})
	if errors.Is(wrap(err), ErrNotFound) {
		return nil
	}
	return wrap(err)
}

func signed(r *oss.PresignResult, err error) (SignedURL, error) {
	if err != nil {
		return SignedURL{}, wrap(err)
	}
	return SignedURL{URL: r.URL, Headers: r.SignedHeaders}, nil
}

func (o *OSS) SignGet(ctx context.Context, key, fileName string, ttl time.Duration) (SignedURL, error) {
	req := &oss.GetObjectRequest{Bucket: &o.cfg.Bucket, Key: &key}
	if fileName != "" {
		cd := ContentDisposition(fileName)
		req.ResponseContentDisposition = &cd
	}
	return signed(o.client.Presign(ctx, req, oss.PresignExpires(ttl)))
}

func (o *OSS) CreateUpload(ctx context.Context, key string) (string, error) {
	res, err := o.client.InitiateMultipartUpload(ctx, &oss.InitiateMultipartUploadRequest{Bucket: &o.cfg.Bucket, Key: &key})
	if err != nil {
		return "", wrap(err)
	}
	return oss.ToString(res.UploadId), nil
}

func (o *OSS) SignPart(ctx context.Context, key, uploadID string, number int, ttl time.Duration) (SignedURL, error) {
	return signed(o.client.Presign(ctx, &oss.UploadPartRequest{Bucket: &o.cfg.Bucket, Key: &key, UploadId: &uploadID,
		PartNumber: int32(number)}, oss.PresignExpires(ttl)))
}

func (o *OSS) CompleteUpload(ctx context.Context, key, uploadID string, parts []Part) error {
	ps := make([]oss.UploadPart, 0, len(parts))
	for _, p := range parts {
		ps = append(ps, oss.UploadPart{PartNumber: int32(p.Number), ETag: oss.Ptr(p.ETag)})
	}
	_, err := o.client.CompleteMultipartUpload(ctx, &oss.CompleteMultipartUploadRequest{Bucket: &o.cfg.Bucket, Key: &key, UploadId: &uploadID,
		CompleteMultipartUpload: &oss.CompleteMultipartUpload{Parts: ps}})
	return wrap(err)
}

func (o *OSS) AbortUpload(ctx context.Context, key, uploadID string) error {
	_, err := o.client.AbortMultipartUpload(ctx, &oss.AbortMultipartUploadRequest{Bucket: &o.cfg.Bucket, Key: &key, UploadId: &uploadID})
	return wrap(err)
}

// Test 写入、读取、删除一个测试对象，检查 AccessKey 有没有需要的权限。
func (o *OSS) Test(ctx context.Context) error {
	key := o.Key(".banbu-connect-test")
	body := "ok " + time.Now().UTC().Format(time.RFC3339)
	if err := o.Put(ctx, key, strings.NewReader(body), int64(len(body))); err != nil {
		return fmt.Errorf("写入测试文件失败：%w", err)
	}
	rc, _, err := o.Open(ctx, key)
	if err != nil {
		return fmt.Errorf("读取测试文件失败：%w", err)
	}
	b, _ := io.ReadAll(io.LimitReader(rc, 1<<10))
	rc.Close()
	if string(b) != body {
		return errors.New("读回的测试文件内容不一致")
	}
	if err := o.Delete(ctx, key); err != nil {
		return fmt.Errorf("删除测试文件失败：%w", err)
	}
	return nil
}

// Origin 是浏览器直传时签名地址的来源（scheme://host），给管理页面的 CSP connect-src 用。
// 用离线签名得到，和实际签出的分片地址一致（含自定义 Endpoint、CNAME）。
func (o *OSS) Origin() string {
	key := o.Key("x")
	r, err := o.client.Presign(context.Background(), &oss.GetObjectRequest{Bucket: &o.cfg.Bucket, Key: &key}, oss.PresignExpires(time.Minute))
	if err != nil {
		return ""
	}
	u, err := url.Parse(r.URL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// 浏览器分片直传需要的 CORS 规则：PUT 分片，读取响应里的 ETag。
var corsMethods = []string{"GET", "PUT", "HEAD"}

// CORSReady 检查桶的 CORS 规则是否允许 origin 直传。读不到规则（没有权限）时返回错误。
func (o *OSS) CORSReady(ctx context.Context, origin string) (bool, error) {
	res, err := o.client.GetBucketCors(ctx, &oss.GetBucketCorsRequest{Bucket: &o.cfg.Bucket})
	if err != nil {
		var se *oss.ServiceError
		if errors.As(err, &se) && se.Code == "NoSuchCORSConfiguration" {
			return false, nil
		}
		return false, wrap(err)
	}
	if res.CORSConfiguration == nil {
		return false, nil
	}
	for _, r := range res.CORSConfiguration.CORSRules {
		if corsAllows(r, origin) {
			return true, nil
		}
	}
	return false, nil
}

func corsAllows(r oss.CORSRule, origin string) bool {
	originOK := slices.Contains(r.AllowedOrigins, "*") || slices.Contains(r.AllowedOrigins, origin)
	methodsOK := true
	for _, m := range []string{"PUT"} {
		methodsOK = methodsOK && slices.Contains(r.AllowedMethods, m)
	}
	headersOK := slices.Contains(r.AllowedHeaders, "*")
	etag := slices.ContainsFunc(r.ExposeHeaders, func(h string) bool { return strings.EqualFold(h, "ETag") })
	return originOK && methodsOK && headersOK && etag
}

// EnsureCORS 在桶的 CORS 规则里加一条允许 origin 直传的规则，保留已有规则。
func (o *OSS) EnsureCORS(ctx context.Context, origin string) error {
	rules := []oss.CORSRule{}
	var vary *bool
	res, err := o.client.GetBucketCors(ctx, &oss.GetBucketCorsRequest{Bucket: &o.cfg.Bucket})
	if err != nil {
		var se *oss.ServiceError
		if !errors.As(err, &se) || se.Code != "NoSuchCORSConfiguration" {
			return wrap(err)
		}
	} else if res.CORSConfiguration != nil {
		rules, vary = res.CORSConfiguration.CORSRules, res.CORSConfiguration.ResponseVary
	}
	for _, r := range rules {
		if corsAllows(r, origin) {
			return nil
		}
	}
	if len(rules) >= 10 {
		return errors.New("桶的 CORS 规则已满 10 条，请在 OSS 控制台手动添加")
	}
	rules = append(rules, oss.CORSRule{AllowedOrigins: []string{origin}, AllowedMethods: corsMethods,
		AllowedHeaders: []string{"*"}, ExposeHeaders: []string{"ETag"}, MaxAgeSeconds: oss.Ptr(int64(600))})
	_, err = o.client.PutBucketCors(ctx, &oss.PutBucketCorsRequest{Bucket: &o.cfg.Bucket,
		CORSConfiguration: &oss.CORSConfiguration{CORSRules: rules, ResponseVary: vary}})
	return wrap(err)
}

// wrap 把 OSS 的错误换成简短的说明。
func wrap(err error) error {
	if err == nil {
		return nil
	}
	var se *oss.ServiceError
	if errors.As(err, &se) {
		if se.StatusCode == 404 && se.Code != "NoSuchBucket" {
			return ErrNotFound // HEAD 请求没有响应体，拿不到错误码
		}
		switch se.Code {
		case "NoSuchKey", "NoSuchUpload":
			return ErrNotFound
		case "NoSuchBucket":
			return errors.New("OSS：Bucket 不存在（检查 Bucket 名称和地域）")
		case "InvalidAccessKeyId":
			return errors.New("OSS：AccessKey ID 无效")
		case "SignatureDoesNotMatch":
			return errors.New("OSS：签名不匹配（检查 AccessKey Secret）")
		case "AccessDenied":
			return fmt.Errorf("OSS：没有权限（%s）", se.Message)
		}
		return fmt.Errorf("OSS：%s（%s）", se.Message, se.Code)
	}
	return fmt.Errorf("OSS：%w", err)
}
