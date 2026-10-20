// Package offsite — копия бэкапов установки self-hosted вне машины, в
// бакете S3 (CLAB-132): выгрузка дампа и архива blobs с контрольной суммой,
// хранение по числу копий, скачивание со сверкой суммы.
//
// Зовёт его подкоманда образа `/api backup-s3` — одна реализация на
// backup.sh, clab server и будущий CronJob Helm: у машины клиента нет ни
// aws, ни rclone, а образ бэкенда уже скачан, подписан и закреплён по
// digest. Тексты — по-английски: их печатают скрипты пакета рядом с
// документацией клиента.
package offsite

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore"
)

// Config — бакет из переменных BACKUP_S3_* (.env установки).
type Config struct {
	Endpoint  string
	Region    string
	Bucket    string
	Prefix    string // без ведущего и с завершающим «/»; пусто — корень бакета
	AccessKey string
	SecretKey string
	PathStyle bool
	Keep      int
}

// DefaultKeep — сколько бэкапов остаётся в бакете по умолчанию.
const DefaultKeep = 14

// FromEnv читает BACKUP_S3_*. Бакет не задан — выгрузки нет (ok == false).
func FromEnv(getenv func(string) string) (cfg Config, ok bool, err error) {
	cfg.Bucket = strings.TrimSpace(getenv("BACKUP_S3_BUCKET"))
	if cfg.Bucket == "" {
		return cfg, false, nil
	}
	cfg.Region = strings.TrimSpace(getenv("BACKUP_S3_REGION"))
	if cfg.Region == "" {
		// Так отвечают MinIO и большинство совместимых хранилищ; у AWS —
		// регион бакета, его задают явно.
		cfg.Region = "us-east-1"
	}
	cfg.Endpoint = strings.TrimSpace(getenv("BACKUP_S3_ENDPOINT"))
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://s3." + cfg.Region + ".amazonaws.com"
	}
	if prefix := strings.Trim(strings.TrimSpace(getenv("BACKUP_S3_PREFIX")), "/"); prefix != "" {
		cfg.Prefix = prefix + "/"
	}
	cfg.AccessKey = getenv("BACKUP_S3_ACCESS_KEY_ID")
	cfg.SecretKey = getenv("BACKUP_S3_SECRET_ACCESS_KEY")
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return cfg, true, errors.New("BACKUP_S3_BUCKET is set, but BACKUP_S3_ACCESS_KEY_ID or BACKUP_S3_SECRET_ACCESS_KEY is empty")
	}
	// Как SDLC_BLOB_S3_PATH_STYLE: MinIO и совместимые умеют только
	// path-style, AWS — оба.
	cfg.PathStyle = true
	if raw := strings.TrimSpace(getenv("BACKUP_S3_PATH_STYLE")); raw != "" {
		v, perr := strconv.ParseBool(raw)
		if perr != nil {
			return cfg, true, fmt.Errorf("BACKUP_S3_PATH_STYLE is '%s', expected true or false", raw)
		}
		cfg.PathStyle = v
	}
	cfg.Keep = DefaultKeep
	if raw := strings.TrimSpace(getenv("BACKUP_S3_KEEP")); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 1 {
			return cfg, true, fmt.Errorf("BACKUP_S3_KEEP is '%s', expected a number of backups, 1 or more", raw)
		}
		cfg.Keep = n
	}
	return cfg, true, nil
}

// Location — s3://bucket/prefix, для сообщений.
func (c Config) Location() string { return "s3://" + c.Bucket + "/" + c.Prefix }

// Store — бэкапы пакета в бакете.
type Store struct {
	cfg      Config
	s3       *blobstore.S3
	PartSize int64
}

// Open — Store по конфигурации; http подменяется в тестах.
func Open(cfg Config, client blobstore.S3Config) (*Store, error) {
	client.Endpoint, client.Region, client.Bucket = cfg.Endpoint, cfg.Region, cfg.Bucket
	client.AccessKey, client.SecretKey, client.PathStyle = cfg.AccessKey, cfg.SecretKey, cfg.PathStyle
	client.Prefix = ""
	s3, err := blobstore.NewS3(client)
	if err != nil {
		return nil, err
	}
	return &Store{cfg: cfg, s3: s3, PartSize: blobstore.DefaultPartSize}, nil
}

// Имена файлов бэкапа — те же, что пишут backup.sh и clab server backup:
// cadence-<version>-<YYYYmmdd-HHMMSS>.sql.gz, .blobs.tar.gz рядом и .sha256
// — суммы обоих в формате sha256sum. Ничего другого хранение не трогает.
const (
	DumpSuffix  = ".sql.gz"
	BlobsSuffix = ".blobs.tar.gz"
	SumSuffix   = ".sha256"
)

var nameRe = regexp.MustCompile(`^(cadence-(v[0-9]+\.[0-9]+\.[0-9]+)-([0-9]{8}-[0-9]{6}))(\.sql\.gz|\.blobs\.tar\.gz|\.sha256)$`)

// Backup — один бэкап в бакете: его объекты по суффиксу.
type Backup struct {
	Stem    string // cadence-vX.Y.Z-YYYYmmdd-HHMMSS
	Version string
	Stamp   string // YYYYmmdd-HHMMSS
	Objects map[string]blobstore.Object
}

// Complete — бэкап выгружен до конца: сумма пишется последней.
func (b Backup) Complete() bool {
	_, dump := b.Objects[DumpSuffix]
	_, sum := b.Objects[SumSuffix]
	return dump && sum
}

// Size — сумма размеров объектов.
func (b Backup) Size() int64 {
	var n int64
	for _, o := range b.Objects {
		n += o.Size
	}
	return n
}

// ParseName разбирает имя файла бэкапа: стем, версия, отметка, суффикс.
func ParseName(name string) (stem, version, stamp, suffix string, ok bool) {
	m := nameRe.FindStringSubmatch(name)
	if m == nil {
		return "", "", "", "", false
	}
	return m[1], m[2], m[3], m[4], true
}

// List — бэкапы под префиксом, новые первыми (по отметке времени в имени,
// не по версии). Объекты с чужими именами в список не попадают.
func (s *Store) List(ctx context.Context) ([]Backup, error) {
	objects, err := s.s3.ListObjects(ctx, s.cfg.Prefix)
	if err != nil {
		return nil, err
	}
	byStem := map[string]*Backup{}
	for _, o := range objects {
		name := strings.TrimPrefix(o.Key, s.cfg.Prefix)
		stem, version, stamp, suffix, ok := ParseName(name)
		if !ok {
			continue
		}
		b := byStem[stem]
		if b == nil {
			b = &Backup{Stem: stem, Version: version, Stamp: stamp, Objects: map[string]blobstore.Object{}}
			byStem[stem] = b
		}
		b.Objects[suffix] = o
	}
	out := make([]Backup, 0, len(byStem))
	for _, b := range byStem {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stamp != out[j].Stamp {
			return out[i].Stamp > out[j].Stamp
		}
		return out[i].Stem > out[j].Stem
	})
	return out, nil
}

// Uploaded — итог выгрузки.
type Uploaded struct {
	Stem    string
	Files   []string // имена выгруженных объектов, сумма последней
	Bytes   int64
	Kept    int
	Removed []string // стемы удалённых бэкапов
}

// Upload выгружает дамп и, если он лежит рядом, архив blobs; затем
// .sha256 с суммами обоих и хранение по Keep. Сумма идёт последней: бэкап
// без неё — недоделанный, его не скачивает get и не считает хранение.
// Сбой выгрузки не трогает старые копии.
func (s *Store) Upload(ctx context.Context, dump string) (Uploaded, error) {
	var res Uploaded
	name := filepath.Base(dump)
	stem, _, _, suffix, ok := ParseName(name)
	if !ok || suffix != DumpSuffix {
		return res, fmt.Errorf("%s is not a backup of the package (cadence-vX.Y.Z-YYYYmmdd-HHMMSS.sql.gz)", name)
	}
	res.Stem = stem
	files := []string{dump}
	if blobs := filepath.Join(filepath.Dir(dump), stem+BlobsSuffix); isFile(blobs) {
		files = append(files, blobs)
	}
	var sums strings.Builder
	for _, f := range files {
		sum, size, err := s.putFile(ctx, f)
		if err != nil {
			return res, err
		}
		fmt.Fprintf(&sums, "%s  %s\n", sum, filepath.Base(f))
		res.Files = append(res.Files, filepath.Base(f))
		res.Bytes += size
	}
	sumName := stem + SumSuffix
	body := []byte(sums.String())
	if err := s.s3.PutObject(ctx, s.cfg.Prefix+sumName, strings.NewReader(sums.String()), int64(len(body)), s.PartSize); err != nil {
		return res, err
	}
	res.Files = append(res.Files, sumName)
	kept, removed, err := s.Prune(ctx)
	res.Kept, res.Removed = kept, removed
	if err != nil {
		return res, fmt.Errorf("the backup is uploaded, but removing old copies failed: %w", err)
	}
	return res, nil
}

// putFile выгружает файл, считая его sha256 по дороге.
func (s *Store) putFile(ctx context.Context, path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	if err := s.s3.PutObject(ctx, s.cfg.Prefix+filepath.Base(path), io.TeeReader(f, h), fi.Size(), s.PartSize); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), fi.Size(), nil
}

// Prune оставляет Keep самых новых полных бэкапов и удаляет всё, что
// старше последнего из них, — и недоделанные выгрузки тоже. Недоделанные
// новее него остаются: это может быть выгрузка, идущая прямо сейчас.
func (s *Store) Prune(ctx context.Context) (kept int, removed []string, err error) {
	backups, err := s.List(ctx)
	if err != nil {
		return 0, nil, err
	}
	complete := 0
	cut := -1
	for i, b := range backups {
		if !b.Complete() {
			continue
		}
		complete++
		if complete == s.cfg.Keep {
			cut = i
			break
		}
	}
	kept = min(complete, s.cfg.Keep)
	if cut < 0 {
		return kept, nil, nil
	}
	for _, b := range backups[cut+1:] {
		// Сумма — первой: прерванное удаление оставляет недоделанный бэкап,
		// а не «полный» без дампа.
		for _, suffix := range []string{SumSuffix, DumpSuffix, BlobsSuffix} {
			o, ok := b.Objects[suffix]
			if !ok {
				continue
			}
			if err := s.s3.DeleteObject(ctx, o.Key); err != nil {
				return kept, removed, err
			}
		}
		removed = append(removed, b.Stem)
	}
	return kept, removed, nil
}

// ErrNone — в бакете нет подходящего бэкапа.
var ErrNone = errors.New("no backup")

// Pick — бэкап для скачивания: по имени (стем или имя дампа), иначе самый
// новый полный — версии version, если она задана.
func (s *Store) Pick(ctx context.Context, name, version string) (Backup, error) {
	backups, err := s.List(ctx)
	if err != nil {
		return Backup{}, err
	}
	if name != "" {
		name = strings.TrimSuffix(filepath.Base(name), DumpSuffix)
	}
	for _, b := range backups {
		switch {
		case name != "":
			if b.Stem != name {
				continue
			}
			if !b.Complete() {
				return Backup{}, fmt.Errorf("%s in %s is incomplete (no %s or %s): its upload did not finish", b.Stem, s.cfg.Location(), DumpSuffix, SumSuffix)
			}
			return b, nil
		case version != "" && b.Version != version:
			continue
		case b.Complete():
			return b, nil
		}
	}
	return Backup{}, ErrNone
}

// Fetched — скачанный бэкап.
type Fetched struct {
	Backup
	Dump  string // путь к дампу
	Blobs string // путь к архиву blobs или пусто
	Bytes int64
}

// Fetch скачивает бэкап в dir и сверяет суммы с его .sha256. Файлы пишутся
// рядом под временными именами и встают на место только со сходящейся
// суммой: испорченная копия не подменяет годный локальный файл.
func (s *Store) Fetch(ctx context.Context, b Backup, dir string) (Fetched, error) {
	res := Fetched{Backup: b}
	var sums strings.Builder
	if err := s.s3.GetObject(ctx, b.Objects[SumSuffix].Key, &sums); err != nil {
		return res, err
	}
	want, err := parseSums(sums.String())
	if err != nil {
		return res, fmt.Errorf("%s%s: %w", b.Stem, SumSuffix, err)
	}
	for _, suffix := range []string{DumpSuffix, BlobsSuffix} {
		o, ok := b.Objects[suffix]
		if !ok {
			continue
		}
		name := b.Stem + suffix
		sum, ok := want[name]
		if !ok {
			return res, fmt.Errorf("%s%s has no checksum of %s", b.Stem, SumSuffix, name)
		}
		path := filepath.Join(dir, name)
		n, err := s.getFile(ctx, o.Key, path, sum)
		if err != nil {
			return res, err
		}
		res.Bytes += n
		if suffix == DumpSuffix {
			res.Dump = path
		} else {
			res.Blobs = path
		}
	}
	return res, nil
}

// ChecksumError — скачанное не сходится с суммой из .sha256.
type ChecksumError struct{ Name, Want, Got string }

func (e *ChecksumError) Error() string {
	return fmt.Sprintf("the checksum of %s does not match: expected %s, got %s - the copy in the bucket is damaged or was changed", e.Name, e.Want, e.Got)
}

func (s *Store) getFile(ctx context.Context, key, path, want string) (int64, error) {
	tmp := path + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(f, h)}
	err = s.s3.GetObject(ctx, key, cw)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp)
		return 0, &ChecksumError{Name: filepath.Base(path), Want: want, Got: got}
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return cw.n, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// parseSums — строки «<sha256>  <имя>», как у sha256sum.
func parseSums(s string) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		sum, name, ok := strings.Cut(line, "  ")
		if !ok || len(sum) != 64 {
			return nil, fmt.Errorf("not a sha256sum line: %q", line)
		}
		out[strings.TrimPrefix(name, "*")] = strings.ToLower(sum)
	}
	if len(out) == 0 {
		return nil, errors.New("no checksums in it")
	}
	return out, nil
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// Location — s3://bucket/prefix хранилища.
func (s *Store) Location() string { return s.cfg.Location() }

// Keep — сколько полных бэкапов держит хранение.
func (s *Store) Keep() int { return s.cfg.Keep }
