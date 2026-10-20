// Package s3test — бакет S3 в памяти для тестов: PUT, GET, HEAD, DELETE,
// ListObjectsV2 с разделителем и страницами, multipart. Каждый запрос
// проверяется подписью SigV4, собранной заново из того, что реально пришло
// по сети: расхождение между подписанным и отправленным ловится здесь, а
// не на настоящем хранилище.
package s3test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fake — бакет Bucket, адресация path-style.
type Fake struct {
	Bucket   string
	Region   string
	Secret   string
	PageSize int // объектов на страницу листинга; 0 — 1000

	mu       sync.Mutex
	Objects  map[string][]byte // ключ → содержимое
	uploads  map[string]map[int][]byte
	nextID   int
	Requests []string // «МЕТОД ключ?запрос»
	// MaxBody — самое большое тело одного запроса: так видно, что выгрузка
	// шла частями, а не одним куском в памяти.
	MaxBody int
	// Fail — ответить ошибкой на запросы, для которых она вернула код.
	Fail func(r *http.Request) int
}

// New — пустой бакет.
func New(bucket, region, secret string) *Fake {
	return &Fake{Bucket: bucket, Region: region, Secret: secret,
		Objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
}

type listResult struct {
	XMLName  xml.Name `xml:"ListBucketResult"`
	Contents []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken,omitempty"`
}

func s3err(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message><AWSAccessKeyId>AKIAEXAMPLE</AWSAccessKeyId></Error>", code, msg)
}

func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(body) > f.MaxBody {
		f.MaxBody = len(body)
	}
	if err := f.verify(r, body); err != nil {
		s3err(w, http.StatusForbidden, "SignatureDoesNotMatch", err.Error())
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/"+f.Bucket)
	if !ok {
		s3err(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist")
		return
	}
	key := strings.TrimPrefix(rest, "/")
	q := r.URL.Query()
	f.Requests = append(f.Requests, r.Method+" "+key+"?"+r.URL.RawQuery)
	if f.Fail != nil {
		if code := f.Fail(r); code != 0 {
			s3err(w, code, "AccessDenied", "Access Denied.")
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && key == "" && q.Get("list-type") == "2":
		f.list(w, q)
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.nextID++
		id := "upload-" + strconv.Itoa(f.nextID)
		f.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>", f.Bucket, key, id)
	case r.Method == http.MethodPut && q.Has("uploadId"):
		parts, ok := f.uploads[q.Get("uploadId")]
		if !ok {
			s3err(w, http.StatusNotFound, "NoSuchUpload", "no such upload")
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		parts[n] = body
		sum := sha256.Sum256(body)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:16])+`"`)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		parts, ok := f.uploads[q.Get("uploadId")]
		if !ok {
			s3err(w, http.StatusNotFound, "NoSuchUpload", "no such upload")
			return
		}
		var done struct {
			Parts []struct {
				PartNumber int    `xml:"PartNumber"`
				ETag       string `xml:"ETag"`
			} `xml:"Part"`
		}
		if err := xml.Unmarshal(body, &done); err != nil {
			s3err(w, http.StatusBadRequest, "MalformedXML", err.Error())
			return
		}
		var all []byte
		for i, p := range done.Parts {
			part, ok := parts[p.PartNumber]
			sum := sha256.Sum256(part)
			if !ok || p.PartNumber != i+1 || p.ETag != `"`+hex.EncodeToString(sum[:16])+`"` {
				s3err(w, http.StatusBadRequest, "InvalidPart", "part "+strconv.Itoa(p.PartNumber))
				return
			}
			if i < len(done.Parts)-1 && len(part) < 5<<20 {
				s3err(w, http.StatusBadRequest, "EntityTooSmall", "part "+strconv.Itoa(p.PartNumber))
				return
			}
			all = append(all, part...)
		}
		f.Objects[key] = all
		delete(f.uploads, q.Get("uploadId"))
		fmt.Fprintf(w, "<CompleteMultipartUploadResult><Key>%s</Key></CompleteMultipartUploadResult>", key)
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(f.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		f.Objects[key] = body
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		content, ok := f.Objects[key]
		if !ok {
			s3err(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
			return
		}
		if r.Method == http.MethodGet {
			w.Write(content)
		}
	case r.Method == http.MethodDelete:
		delete(f.Objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		s3err(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

func (f *Fake) list(w http.ResponseWriter, q url.Values) {
	prefix, delim := q.Get("prefix"), q.Get("delimiter")
	var keys []string
	prefixes := map[string]bool{}
	for k := range f.Objects {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if delim != "" {
			if i := strings.Index(k[len(prefix):], delim); i >= 0 {
				prefixes[k[:len(prefix)+i+1]] = true
				continue
			}
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	start := 0
	if token := q.Get("continuation-token"); token != "" {
		start, _ = strconv.Atoi(token)
	}
	page := f.PageSize
	if page <= 0 {
		page = 1000
	}
	var res listResult
	end := min(start+page, len(keys))
	for _, k := range keys[start:end] {
		res.Contents = append(res.Contents, struct {
			Key          string `xml:"Key"`
			Size         int64  `xml:"Size"`
			LastModified string `xml:"LastModified"`
		}{k, int64(len(f.Objects[k])), time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC).Format(time.RFC3339)})
	}
	for p := range prefixes {
		res.CommonPrefixes = append(res.CommonPrefixes, struct {
			Prefix string `xml:"Prefix"`
		}{p})
	}
	if end < len(keys) {
		res.IsTruncated = true
		res.NextContinuationToken = strconv.Itoa(end)
	}
	out, _ := xml.Marshal(res)
	w.Write(out)
}

// Keys — ключи бакета по порядку.
func (f *Fake) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.Objects))
	for k := range f.Objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Put кладёт объект в обход подписи — чужой объект бакета в тесте.
func (f *Fake) Put(key string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Objects[key] = content
}

// Get — содержимое объекта.
func (f *Fake) Get(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.Objects[key]
	return b, ok
}

func (f *Fake) verify(r *http.Request, body []byte) error {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		return errors.New("no signature")
	}
	sum := sha256.Sum256(body)
	if got := r.Header.Get("X-Amz-Content-Sha256"); got != hex.EncodeToString(sum[:]) {
		return errors.New("x-amz-content-sha256 does not match the body")
	}
	var credential, signedHeaders, signature string
	for _, part := range strings.Split(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 "), ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch name {
		case "Credential":
			credential = value
		case "SignedHeaders":
			signedHeaders = value
		case "Signature":
			signature = value
		}
	}
	_, scope, ok := strings.Cut(credential, "/")
	if !ok {
		return errors.New("broken Credential")
	}
	var headers strings.Builder
	for _, name := range strings.Split(signedHeaders, ";") {
		value := r.Header.Get(name)
		if name == "host" {
			value = r.Host
		}
		headers.WriteString(name + ":" + strings.TrimSpace(value) + "\n")
	}
	// Канонический запрос — из разобранных параметров, как его строит S3,
	// а не из строки, которую прислал клиент.
	canonical := strings.Join([]string{
		r.Method, canonicalPath(r.URL.Path), canonicalQuery(r.URL.Query()),
		headers.String(), signedHeaders, hex.EncodeToString(sum[:]),
	}, "\n")
	hash := sha256.Sum256([]byte(canonical))
	stamp := r.Header.Get("X-Amz-Date")
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", stamp, scope, hex.EncodeToString(hash[:])}, "\n")
	key := mac([]byte("AWS4"+f.Secret), stamp[:8])
	key = mac(key, f.Region)
	key = mac(key, "s3")
	key = mac(key, "aws4_request")
	if hex.EncodeToString(mac(key, toSign)) != signature {
		return errors.New("the signature does not match")
	}
	return nil
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, enc(k)+"="+enc(v))
		}
	}
	return strings.Join(parts, "&")
}

func canonicalPath(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = enc(s)
	}
	return strings.Join(segments, "/")
}

// enc — RFC 3986: всё, кроме A-Z a-z 0-9 - _ . ~, в %XX.
func enc(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func mac(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
