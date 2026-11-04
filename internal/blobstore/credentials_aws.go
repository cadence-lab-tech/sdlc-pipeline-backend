package blobstore

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Источники временных ключей AWS для пода в EKS (CLAB-394). Это два
// HTTP-запроса и разбор ответа, поэтому они написаны здесь же, как и подпись:
// AWS SDK ради них в зависимости не идёт.

// Имена источников — в логе старта и в сообщениях об ошибках.
const (
	SourceStatic      = "static"
	SourcePodIdentity = "pod-identity"
	SourceIRSA        = "irsa"
)

// errBodyMax — сколько тела ответа об ошибке попадает в сообщение.
const errBodyMax = 512

// containerAgentHosts — адреса агента Pod Identity (и ECS), к которым токен
// можно слать по открытому http: они не покидают узел.
var containerAgentHosts = map[string]bool{"169.254.170.23": true, "fd00:ec2::23": true, "169.254.170.2": true}

// podIdentity — агент EKS Pod Identity: GET по AWS_CONTAINER_CREDENTIALS_FULL_URI
// с токеном из AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE в Authorization. Файл
// читается на каждом обновлении: kubelet ротирует проекционный токен.
type podIdentity struct {
	uri       string
	tokenFile string
	token     string // AWS_CONTAINER_AUTHORIZATION_TOKEN, если файла нет
	client    *http.Client
}

func newPodIdentity(uri, tokenFile, token string) (*podIdentity, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("blobstore: AWS_CONTAINER_CREDENTIALS_FULL_URI %q — не адрес", uri)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && localAgent(u.Hostname())) {
		// Токен пода не уходит по открытому каналу за пределы узла.
		return nil, fmt.Errorf("blobstore: AWS_CONTAINER_CREDENTIALS_FULL_URI %q: по http допустимы только "+
			"loopback и адреса агента 169.254.170.23 и fd00:ec2::23, иначе нужен https", uri)
	}
	// Агент живёт на узле: прокси из окружения до него не довезёт, а токен
	// пода прокси видеть незачем.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &podIdentity{uri: uri, tokenFile: tokenFile, token: token,
		client: &http.Client{Transport: transport, Timeout: fetchTimeout}}, nil
}

func localAgent(host string) bool {
	if host == "localhost" || containerAgentHosts[host] {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (p *podIdentity) fetch(ctx context.Context) (Credentials, error) {
	fail := func(format string, a ...any) (Credentials, error) {
		// Адрес — аргумент, а не часть формата: %41 в нём не глагол.
		return Credentials{}, fmt.Errorf("blobstore: учётные данные S3 (%s): %s: %s", SourcePodIdentity, p.uri, fmt.Sprintf(format, a...))
	}
	token := p.token
	if p.tokenFile != "" {
		var err error
		if token, err = readToken(p.tokenFile); err != nil {
			return Credentials{}, fmt.Errorf("blobstore: учётные данные S3 (%s): %w", SourcePodIdentity, err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.uri, nil)
	if err != nil {
		return fail("%v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fail("%v", unwrapURLError(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fail("ответ %s: %s", resp.Status, clip(body))
	}
	var out struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		Token           string `json:"Token"`
		Expiration      time.Time
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessKeyID == "" || out.SecretAccessKey == "" {
		return fail("ответ %s без AccessKeyId и SecretAccessKey", resp.Status)
	}
	return Credentials{AccessKeyID: out.AccessKeyID, SecretAccessKey: out.SecretAccessKey,
		SessionToken: out.Token, Expires: out.Expiration}, nil
}

// webIdentity — IRSA: неподписанный AssumeRoleWithWebIdentity в STS с токеном
// ServiceAccount из AWS_WEB_IDENTITY_TOKEN_FILE. Токен — в теле POST, а не в
// адресе: адреса оседают в логах прокси.
type webIdentity struct {
	endpoint  string
	roleARN   string
	session   string
	tokenFile string
	client    *http.Client
}

func newWebIdentity(endpoint, roleARN, session, tokenFile string) *webIdentity {
	if session == "" {
		session = fmt.Sprintf("cadence-lab-%d", time.Now().UnixNano())
	}
	// STS — обычный исходящий адрес: прокси из окружения ему положен.
	return &webIdentity{endpoint: endpoint, roleARN: roleARN, session: session, tokenFile: tokenFile,
		client: &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone(), Timeout: fetchTimeout}}
}

// stsEndpoint — STS региона, если адрес не задан явно.
func stsEndpoint(region string) string {
	if strings.HasPrefix(region, "cn-") {
		return "https://sts." + region + ".amazonaws.com.cn"
	}
	return "https://sts." + region + ".amazonaws.com"
}

func (w *webIdentity) fetch(ctx context.Context) (Credentials, error) {
	fail := func(format string, a ...any) (Credentials, error) {
		return Credentials{}, fmt.Errorf("blobstore: учётные данные S3 (%s): STS %s: %s", SourceIRSA, w.endpoint, fmt.Sprintf(format, a...))
	}
	token, err := readToken(w.tokenFile)
	if err != nil {
		return Credentials{}, fmt.Errorf("blobstore: учётные данные S3 (%s): %w", SourceIRSA, err)
	}
	form := url.Values{
		"Action":           {"AssumeRoleWithWebIdentity"},
		"Version":          {"2011-06-15"},
		"RoleArn":          {w.roleARN},
		"RoleSessionName":  {w.session},
		"WebIdentityToken": {token},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fail("%v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := w.client.Do(req)
	if err != nil {
		return fail("%v", unwrapURLError(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code    string `xml:"Error>Code"`
			Message string `xml:"Error>Message"`
		}
		if xml.Unmarshal(body, &e) == nil && e.Code != "" {
			return fail("ответ %s: %s: %s", resp.Status, e.Code, clip([]byte(e.Message)))
		}
		return fail("ответ %s: %s", resp.Status, clip(body))
	}
	var out struct {
		AccessKeyID     string    `xml:"AssumeRoleWithWebIdentityResult>Credentials>AccessKeyId"`
		SecretAccessKey string    `xml:"AssumeRoleWithWebIdentityResult>Credentials>SecretAccessKey"`
		SessionToken    string    `xml:"AssumeRoleWithWebIdentityResult>Credentials>SessionToken"`
		Expiration      time.Time `xml:"AssumeRoleWithWebIdentityResult>Credentials>Expiration"`
	}
	if err := xml.Unmarshal(body, &out); err != nil || out.AccessKeyID == "" || out.SecretAccessKey == "" {
		return fail("ответ %s без AccessKeyId и SecretAccessKey", resp.Status)
	}
	return Credentials{AccessKeyID: out.AccessKeyID, SecretAccessKey: out.SecretAccessKey,
		SessionToken: out.SessionToken, Expires: out.Expiration}, nil
}

// readToken читает файл токена целиком; пустой файл — ошибка: с пустым
// токеном агент и STS отвечают отказом, по которому не понять причину.
func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("файл токена %s: %w", path, unwrapPathError(err))
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("файл токена %s пуст", path)
	}
	return token, nil
}

func unwrapPathError(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// unwrapURLError снимает с ошибки транспорта адрес: он уже назван в
// сообщении, а у STS в адресе мог бы оказаться запрос.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func clip(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > errBodyMax {
		s = s[:errBodyMax] + "…"
	}
	return s
}

// EnvCredentials выбирает источник ключей по окружению: явные статические
// ключи → Pod Identity → IRSA. Порядок отличается от AWS SDK (там web
// identity раньше container credentials): заказчику первым нужен Pod
// Identity, и под, где по ошибке выставлено и то и другое, работает по нему.
// Половина статической пары — отказ, а не молчаливый переход к следующему:
// администратор хотел ключи и опечатался.
func EnvCredentials(cfg S3Config, getenv func(string) string, log *slog.Logger) (CredentialsProvider, string, error) {
	switch {
	case cfg.AccessKey != "" && cfg.SecretKey != "":
		return StaticCredentials{AccessKeyID: cfg.AccessKey, SecretAccessKey: cfg.SecretKey,
			SessionToken: cfg.SessionToken}, SourceStatic, nil
	case cfg.AccessKey != "":
		return nil, "", errors.New("blobstore: задан SDLC_BLOB_S3_ACCESS_KEY_ID, но не задан " +
			"SDLC_BLOB_S3_SECRET_ACCESS_KEY (ни значением, ни файлом _FILE, ни AWS_SECRET_ACCESS_KEY)")
	case cfg.SecretKey != "":
		return nil, "", errors.New("blobstore: задан SDLC_BLOB_S3_SECRET_ACCESS_KEY, но не задан " +
			"SDLC_BLOB_S3_ACCESS_KEY_ID (ни значением, ни файлом _FILE, ни AWS_ACCESS_KEY_ID)")
	case cfg.SessionToken != "":
		return nil, "", errors.New("blobstore: задан SDLC_BLOB_S3_SESSION_TOKEN (или AWS_SESSION_TOKEN) без пары " +
			"SDLC_BLOB_S3_ACCESS_KEY_ID и SDLC_BLOB_S3_SECRET_ACCESS_KEY")
	}
	if uri := getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI"); uri != "" {
		p, err := newPodIdentity(uri, getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"), getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN"))
		if err != nil {
			return nil, "", err
		}
		return newRefreshing(SourcePodIdentity, p.fetch, log), SourcePodIdentity, nil
	}
	tokenFile, role := getenv("AWS_WEB_IDENTITY_TOKEN_FILE"), getenv("AWS_ROLE_ARN")
	switch {
	case tokenFile != "" && role != "":
		endpoint := cfg.STSEndpoint
		if endpoint == "" {
			endpoint = stsEndpoint(cfg.Region)
		}
		w := newWebIdentity(endpoint, role, getenv("AWS_ROLE_SESSION_NAME"), tokenFile)
		return newRefreshing(SourceIRSA, w.fetch, log), SourceIRSA, nil
	case tokenFile != "":
		return nil, "", errors.New("blobstore: IRSA: задан AWS_WEB_IDENTITY_TOKEN_FILE, но не задан AWS_ROLE_ARN")
	case role != "":
		return nil, "", errors.New("blobstore: IRSA: задан AWS_ROLE_ARN, но не задан AWS_WEB_IDENTITY_TOKEN_FILE")
	}
	return nil, "", errors.New("blobstore: ключи S3 не заданы: укажите SDLC_BLOB_S3_ACCESS_KEY_ID и " +
		"SDLC_BLOB_S3_SECRET_ACCESS_KEY, либо настройте EKS Pod Identity (AWS_CONTAINER_CREDENTIALS_FULL_URI и " +
		"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE), либо IRSA (AWS_WEB_IDENTITY_TOKEN_FILE и AWS_ROLE_ARN)")
}
