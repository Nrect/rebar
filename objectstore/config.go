package objectstore

import (
	"fmt"
	"slices"
	"time"
)

// MaxPresignTTL — потолок жизни подписанной ссылки: предел SigV4 в семь суток.
const MaxPresignTTL = 7 * 24 * time.Hour

// UploaderConfig — политика приёма файлов. Нулевое значение любого поля —
// отказ на старте, а не «без ограничений».
//
// ПОЛЯ «ПРИНИМАТЬ SVG» ЗДЕСЬ НЕТ И НЕ БУДЕТ: SVG — документ со скриптами, и
// отданный с нашего домена он отдаёт чужой JS в нашей origin (ADR-0006, п. 3).
// Флаг, выключающий инвариант, однажды окажется включённым в проде.
type UploaderConfig struct {
	// Prefix — каталог ключа: <Prefix>/<uuid>.<ext>.
	Prefix string
	// MaxSize — потолок тела в байтах. Он же потолок памяти на одну загрузку:
	// тело принимается целиком в память, чтобы отказ случился до записи в
	// хранилище (см. doc.go, «Чего в пакете НЕТ»).
	MaxSize int64
	// Accept — какие из AllContentTypes принимать. Подмножество, а не свободный
	// список: тип без расширения в белом списке принять нечем.
	Accept []ContentType
}

func (c UploaderConfig) validate() error {
	if err := checkPrefix("UploaderConfig.Prefix", c.Prefix); err != nil {
		return err
	}
	if c.MaxSize <= 0 {
		return fmt.Errorf("UploaderConfig.MaxSize must be positive, got %d", c.MaxSize)
	}
	if len(c.Accept) == 0 {
		return fmt.Errorf("UploaderConfig.Accept must not be empty, allowed types are %v", AllContentTypes)
	}
	seen := make(map[ContentType]bool, len(c.Accept))
	for _, ct := range c.Accept {
		if !ct.valid() {
			return fmt.Errorf("UploaderConfig.Accept: type %q must be one of %v", ct, AllContentTypes)
		}
		if seen[ct] {
			return fmt.Errorf("UploaderConfig.Accept: type %q is listed twice", ct)
		}
		seen[ct] = true
	}
	return nil
}

func (c UploaderConfig) accepts(ct ContentType) bool {
	return slices.Contains(c.Accept, ct)
}

// CollectMode — что сборщик делает с найденной сиротой. Закрытый набор, а не
// булев DryRun: нулевое значение bool — это `false`, то есть УДАЛЯТЬ, и
// забытое поле снимало бы предохранитель молча (CONVENTIONS §2, fail-closed).
type CollectMode string

const (
	// CollectDryRun — только считать. Первый прогон у нового потребителя.
	CollectDryRun CollectMode = "dry_run"
	// CollectDelete — считать и удалять.
	CollectDelete CollectMode = "delete"
)

// AllCollectModes — полный список; держит guard-тест.
var AllCollectModes = []CollectMode{CollectDryRun, CollectDelete}

func (m CollectMode) valid() bool { return m == CollectDryRun || m == CollectDelete }

// CollectorConfig — политика уборки сирот. Оба предохранителя обязательны.
type CollectorConfig struct {
	// Prefix — какой каталог обходить.
	Prefix string
	// MinAge — grace: объект моложе не трогается никогда. Между Put и вставкой
	// строки потребителя есть окно, и без grace сборщик удалял бы файлы,
	// которые прямо сейчас загружают.
	MinAge time.Duration
	// Mode — считать или удалять; см. CollectMode.
	Mode CollectMode
	// BatchSize — ключей за один List.
	BatchSize int
}

func (c CollectorConfig) validate() error {
	if err := checkPrefix("CollectorConfig.Prefix", c.Prefix); err != nil {
		return err
	}
	if c.MinAge <= 0 {
		return fmt.Errorf("CollectorConfig.MinAge must be positive, got %s", c.MinAge)
	}
	if !c.Mode.valid() {
		return fmt.Errorf("CollectorConfig.Mode must be one of %v", AllCollectModes)
	}
	if c.BatchSize <= 0 {
		return fmt.Errorf("CollectorConfig.BatchSize must be positive, got %d", c.BatchSize)
	}
	return nil
}
