package blobstore

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Операции над объектами по их полному ключу — для выгрузки бэкапов
// (CLAB-132), а не для содержимого артефактов: префикс хранилища здесь не
// подставляется, ключ называет вызывающий. Подпись та же, SigV4 на
// стандартной библиотеке; к PUT, GET, HEAD и DELETE добавлены листинг
// (ListObjectsV2) и multipart — дамп в гигабайты одним PUT в памяти не
// держат, а у AWS одиночный PUT ограничен 5 ГБ.

// MinPartSize — меньше части multipart S3 не принимает (кроме последней).
const MinPartSize = 5 << 20

// DefaultPartSize — часть по умолчанию: память процесса — одна часть, а
// 10 000 частей (предел S3) дают 160 ГБ.
const DefaultPartSize = 16 << 20

// maxParts — предел S3 на число частей одной выгрузки.
const maxParts = 10000

// Object — объект из листинга.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// ObjectError — отказ хранилища: HTTP-статус и код S3. Ключей доступа в
// тексте нет: тело ответа разбирается до Code и Message, а не печатается
// целиком.
type ObjectError struct {
	Op      string
	Key     string
	Status  string
	Code    string
	Message string
}

func (e *ObjectError) Error() string {
	s := e.Op + " " + e.Key + ": " + e.Status
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Message != "" {
		s += " (" + e.Message + ")"
	}
	return s
}

func objectError(op, key string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var parsed struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	_ = xml.Unmarshal(body, &parsed)
	return &ObjectError{Op: op, Key: key, Status: resp.Status, Code: parsed.Code, Message: parsed.Message}
}

// PutObject выгружает size байт из r под ключом key. До partSize — одним
// PUT, больше — multipart частями по partSize; в памяти одновременно не
// больше одной части. Недоделанная multipart-выгрузка отменяется.
func (s *S3) PutObject(ctx context.Context, key string, r io.Reader, size, partSize int64) error {
	if partSize <= 0 {
		partSize = DefaultPartSize
	}
	if partSize < MinPartSize {
		return fmt.Errorf("blobstore: part size %d is below the S3 minimum of %d bytes", partSize, MinPartSize)
	}
	// Части растут, чтобы уложиться в предел их числа.
	for size/partSize >= maxParts {
		partSize *= 2
	}
	if size <= partSize {
		body := make([]byte, size)
		if _, err := io.ReadFull(r, body); err != nil {
			return fmt.Errorf("blobstore: reading %s: %w", key, err)
		}
		return s.putOne(ctx, key, nil, body)
	}
	return s.putMultipart(ctx, key, r, size, partSize)
}

func (s *S3) putOne(ctx context.Context, key string, query url.Values, body []byte) error {
	req, err := s.requestQuery(ctx, http.MethodPut, key, query, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := s.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return objectError("PUT", key, resp)
	}
	return nil
}

type completeMultipart struct {
	XMLName xml.Name        `xml:"CompleteMultipartUpload"`
	Parts   []completedPart `xml:"Part"`
}

type completedPart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

func (s *S3) putMultipart(ctx context.Context, key string, r io.Reader, size, partSize int64) (err error) {
	uploadID, err := s.createMultipart(ctx, key)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			// Брошенные части занимают место в бакете, пока их не отменят.
			s.abortMultipart(context.WithoutCancel(ctx), key, uploadID)
		}
	}()
	buf := make([]byte, partSize)
	var parts []completedPart
	for n, left := 1, size; left > 0; n++ {
		chunk := buf[:min(left, partSize)]
		if _, err := io.ReadFull(r, chunk); err != nil {
			return fmt.Errorf("blobstore: reading %s: %w", key, err)
		}
		left -= int64(len(chunk))
		etag, err := s.uploadPart(ctx, key, uploadID, n, chunk)
		if err != nil {
			return err
		}
		parts = append(parts, completedPart{PartNumber: n, ETag: etag})
	}
	return s.completeMultipart(ctx, key, uploadID, parts)
}

func (s *S3) createMultipart(ctx context.Context, key string) (string, error) {
	req, err := s.requestQuery(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := s.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", objectError("POST (start multipart)", key, resp)
	}
	var result struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil || result.UploadID == "" {
		return "", fmt.Errorf("blobstore: start multipart %s: no UploadId in the answer", key)
	}
	return result.UploadID, nil
}

func (s *S3) uploadPart(ctx context.Context, key, uploadID string, n int, body []byte) (string, error) {
	q := url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {uploadID}}
	req, err := s.requestQuery(ctx, http.MethodPut, key, q, body)
	if err != nil {
		return "", err
	}
	resp, err := s.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", objectError(fmt.Sprintf("PUT (part %d)", n), key, resp)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", fmt.Errorf("blobstore: part %d of %s: no ETag in the answer", n, key)
	}
	return etag, nil
}

func (s *S3) completeMultipart(ctx context.Context, key, uploadID string, parts []completedPart) error {
	body, err := xml.Marshal(completeMultipart{Parts: parts})
	if err != nil {
		return err
	}
	req, err := s.requestQuery(ctx, http.MethodPost, key, url.Values{"uploadId": {uploadID}}, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/xml")
	resp, err := s.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return objectError("POST (complete multipart)", key, resp)
	}
	// S3 может ответить 200 и ошибкой в теле, если сборка частей упала
	// после начала ответа.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if bytes.Contains(raw, []byte("<Error>")) {
		var e struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		_ = xml.Unmarshal(raw, &e)
		return &ObjectError{Op: "POST (complete multipart)", Key: key, Status: resp.Status, Code: e.Code, Message: e.Message}
	}
	return nil
}

func (s *S3) abortMultipart(ctx context.Context, key, uploadID string) {
	req, err := s.requestQuery(ctx, http.MethodDelete, key, url.Values{"uploadId": {uploadID}}, nil)
	if err != nil {
		return
	}
	if resp, err := s.do(req); err == nil {
		resp.Body.Close()
	}
}

// GetObject пишет содержимое объекта в w потоком. Нет объекта — ErrNotFound.
func (s *S3) GetObject(ctx context.Context, key string, w io.Writer) error {
	req, err := s.request(ctx, http.MethodGet, key, nil)
	if err != nil {
		return err
	}
	resp, err := s.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return objectError("GET", key, resp)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("blobstore: GET %s: %w", key, err)
	}
	return nil
}

// DeleteObject удаляет объект; удаление отсутствующего — не ошибка.
func (s *S3) DeleteObject(ctx context.Context, key string) error {
	req, err := s.request(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	resp, err := s.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	default:
		return objectError("DELETE", key, resp)
	}
}

// ListObjects — объекты прямо под префиксом (ListObjectsV2 с разделителем
// «/»): вложенные «каталоги» не перечисляются, их объекты не трогаются.
func (s *S3) ListObjects(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "delimiter": {"/"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		req, err := s.requestQuery(ctx, http.MethodGet, "", q, nil)
		if err != nil {
			return nil, err
		}
		resp, err := s.do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			err := objectError("LIST", s.cfg.Bucket+"/"+prefix, resp)
			resp.Body.Close()
			return nil, err
		}
		var page struct {
			Contents []struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("blobstore: LIST %s: %w", prefix, err)
		}
		for _, c := range page.Contents {
			out = append(out, Object{Key: c.Key, Size: c.Size, LastModified: c.LastModified})
		}
		if !page.IsTruncated {
			return out, nil
		}
		if page.NextContinuationToken == "" {
			return nil, errors.New("blobstore: LIST: truncated without a continuation token")
		}
		token = page.NextContinuationToken
	}
}

// Bucket — имя бакета, для сообщений.
func (s *S3) Bucket() string { return s.cfg.Bucket }

// ObjectURI — s3://bucket/key, для сообщений.
func (s *S3) ObjectURI(key string) string {
	return s3Scheme + s.cfg.Bucket + "/" + strings.TrimPrefix(key, "/")
}
