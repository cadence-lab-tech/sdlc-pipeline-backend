package embed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
)

// probeText — что уходит модели при пробе. Короткое: проба проверяет
// размерность и доступ, а не качество.
const probeText = "Cadence Lab: проба размерности"

// probeTimeout — сколько старт ждёт ответа пробы. Недоступная модель старт не
// останавливает, поэтому и держать его долго незачем.
var probeTimeout = 20 * time.Second

// prober — провайдер, умеющий один запрос без повторов. Проба при старте не
// должна ждать ретраев с экспоненциальной паузой: на недоступной модели это
// десятки секунд до того, как сервис начнёт отвечать.
type prober interface {
	probe(ctx context.Context) error
}

// New собирает эмбеддер по настройке и проверяет его одной пробой. Это
// единственное место выбора провайдера: api считает вектор запроса, воркер —
// векторы кусков, и разойтись в выборе им нельзя.
//
// none — nil и ни одного запроса: поиск остаётся полнотекстовым, это рабочий
// режим, а не поломка. Для HTTP-провайдеров проба:
//   - вектор не той размерности — ошибка, процесс не стартует: со схемой
//     vector(1024) такая модель не заработает никогда (решение 0008);
//   - неверный ключ, неизвестная модель, отбитый запрос — ошибка: это
//     настройка, и узнать о ней на старте дешевле, чем по пустому поиску;
//   - сеть, таймаут, 429 и 5xx — предупреждение и рабочий эмбеддер: модель
//     может подняться позже, а индексация и поиск без неё деградируют, но живут.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (Embedder, error) {
	var e Embedder
	switch cfg.EmbeddingProvider {
	case "", "none":
		log.Info("эмбеддинги выключены: поиск полнотекстовый", "provider", "none")
		return nil, nil
	case "hash":
		log.Warn("эмбеддинги считает заглушка: это мешок слов, а не семантика",
			"provider", "hash", "model", Hash{}.Model())
		return Hash{}, nil
	case "voyage":
		e = NewVoyage(cfg.VoyageAPIURL, cfg.VoyageAPIKey, cfg.EmbeddingModel, cfg.EmbeddingBatch)
	case "openai":
		e = NewOpenAI(cfg.EmbeddingURL, cfg.EmbeddingAPIKey, cfg.EmbeddingModel,
			cfg.EmbeddingBatch, cfg.EmbeddingSendDimensions)
	default:
		return nil, fmt.Errorf("провайдер эмбеддингов %q — не voyage, openai, hash или none", cfg.EmbeddingProvider)
	}

	if err := Probe(ctx, e); err != nil {
		var dim *DimensionError
		switch {
		case errors.As(err, &dim):
			return nil, fmt.Errorf("эмбеддинги: модель %s (%s) отдаёт вектор %d, схема требует %d — "+
				"нужна модель, которая отдаёт %d (решение 0008)",
				e.Model(), cfg.EmbeddingProvider, dim.Got, Dim, Dim)
		case transient(err):
			log.Warn("эмбеддинги: модель не ответила на пробу, стартуем без проверки размерности",
				"provider", cfg.EmbeddingProvider, "model", e.Model(), "error", err)
		default:
			return nil, fmt.Errorf("эмбеддинги: проба модели %s (%s) не прошла: %w",
				e.Model(), cfg.EmbeddingProvider, err)
		}
	}
	log.Info("эмбеддинги", "provider", cfg.EmbeddingProvider, "model", e.Model())
	return e, nil
}

// Probe отправляет модели один короткий текст и проверяет длину вектора.
func Probe(ctx context.Context, e Embedder) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if p, ok := e.(prober); ok {
		return p.probe(ctx)
	}
	vectors, err := e.Embed(ctx, []string{probeText}, KindQuery)
	if err != nil {
		return err
	}
	if len(vectors) != 1 {
		return fmt.Errorf("модель вернула %d векторов на один текст", len(vectors))
	}
	if len(vectors[0]) != Dim {
		return &DimensionError{Provider: e.Model(), Got: len(vectors[0])}
	}
	return nil
}
