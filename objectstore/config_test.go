package objectstore_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/objectstore/objectstoretest"
)

// Нулевое значение конфига — отказ на старте, а не «без ограничений»: забытое
// поле не имеет права тихо снять защиту (CONVENTIONS §2).
func TestUploaderConfig_EveryFieldIsRequired(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mutate func(*objectstore.UploaderConfig)
		want   string
	}{
		"нулевой конфиг":        {mutate: func(c *objectstore.UploaderConfig) { *c = objectstore.UploaderConfig{} }, want: "UploaderConfig.Prefix must not be empty"},
		"пустой префикс":        {mutate: func(c *objectstore.UploaderConfig) { c.Prefix = "" }, want: "UploaderConfig.Prefix must not be empty"},
		"префикс со слэшем":     {mutate: func(c *objectstore.UploaderConfig) { c.Prefix = "/uploads" }, want: "UploaderConfig.Prefix must not start or end with a slash"},
		"префикс с обходом":     {mutate: func(c *objectstore.UploaderConfig) { c.Prefix = "uploads/.." }, want: "UploaderConfig.Prefix must be a valid key prefix"},
		"нулевой потолок":       {mutate: func(c *objectstore.UploaderConfig) { c.MaxSize = 0 }, want: "UploaderConfig.MaxSize must be positive"},
		"отрицательный потолок": {mutate: func(c *objectstore.UploaderConfig) { c.MaxSize = -1 }, want: "UploaderConfig.MaxSize must be positive"},
		"пустой Accept":         {mutate: func(c *objectstore.UploaderConfig) { c.Accept = nil }, want: "UploaderConfig.Accept must not be empty"},
		"чужой тип":             {mutate: func(c *objectstore.UploaderConfig) { c.Accept = []objectstore.ContentType{"image/tiff"} }, want: `UploaderConfig.Accept: type "image/tiff" must be one of`},
		"тип дважды": {mutate: func(c *objectstore.UploaderConfig) {
			c.Accept = []objectstore.ContentType{objectstore.ContentTypePNG, objectstore.ContentTypePNG}
		}, want: `UploaderConfig.Accept: type "image/png" is listed twice`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := testUploaderConfig()
			tc.mutate(&cfg)

			assert.Panics(t, func() { objectstore.NewUploader(objectstoretest.NewMemStore(), cfg) })
			assert.Contains(t, recoverText(func() {
				objectstore.NewUploader(objectstoretest.NewMemStore(), cfg)
			}), tc.want)
		})
	}
}

func TestCollectorConfig_EveryFieldIsRequired(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mutate func(*objectstore.CollectorConfig)
		want   string
	}{
		"нулевой конфиг":      {mutate: func(c *objectstore.CollectorConfig) { *c = objectstore.CollectorConfig{} }, want: "CollectorConfig.Prefix must not be empty"},
		"нулевой grace":       {mutate: func(c *objectstore.CollectorConfig) { c.MinAge = 0 }, want: "CollectorConfig.MinAge must be positive"},
		"отрицательный grace": {mutate: func(c *objectstore.CollectorConfig) { c.MinAge = -time.Second }, want: "CollectorConfig.MinAge must be positive"},
		"режим не назван":     {mutate: func(c *objectstore.CollectorConfig) { c.Mode = "" }, want: "CollectorConfig.Mode must be one of [dry_run delete]"},
		"чужой режим":         {mutate: func(c *objectstore.CollectorConfig) { c.Mode = "purge" }, want: "CollectorConfig.Mode must be one of [dry_run delete]"},
		"нулевая пачка":       {mutate: func(c *objectstore.CollectorConfig) { c.BatchSize = 0 }, want: "CollectorConfig.BatchSize must be positive"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := testCollectorConfig(objectstore.CollectDelete)
			tc.mutate(&cfg)

			assert.Panics(t, func() {
				objectstore.NewCollector(objectstoretest.NewMemStore(), objectstoretest.NewMemOwned(), cfg)
			})
			assert.Contains(t, recoverText(func() {
				objectstore.NewCollector(objectstoretest.NewMemStore(), objectstoretest.NewMemOwned(), cfg)
			}), tc.want)
		})
	}
}

// НЕНАЗВАННЫЙ РЕЖИМ СБОРЩИКА — ОТКАЗ, А НЕ УДАЛЕНИЕ. Будь это булев DryRun,
// нулевое значение означало бы «удалять», и забытое поле снимало бы
// предохранитель молча.
func TestCollectorConfig_ZeroModeIsRefusalNotDeletion(t *testing.T) {
	t.Parallel()
	cfg := objectstore.CollectorConfig{Prefix: testPrefix, MinAge: time.Hour, BatchSize: 10}

	assert.NotContains(t, objectstore.AllCollectModes, objectstore.CollectMode(""))
	assert.Panics(t, func() {
		objectstore.NewCollector(objectstoretest.NewMemStore(), objectstoretest.NewMemOwned(), cfg)
	})
}

// Флагов, выключающих инвариант, в конфигах нет: режим разработки достигается
// подменой порта (fs вместо s3), а не полем.
func TestConfigs_HaveNoInvariantSwitches(t *testing.T) {
	t.Parallel()

	for _, field := range fieldNames(objectstore.UploaderConfig{}, objectstore.CollectorConfig{}) {
		for _, forbidden := range []string{"Skip", "Trust", "Allow", "Insecure", "Disable"} {
			assert.NotContains(t, field, forbidden,
				"поле %q похоже на выключатель инварианта: режим разработки — подмена порта, а не флаг", field)
		}
	}
}

// fieldNames — имена полей перечисленных структур.
func fieldNames(values ...any) []string {
	var names []string
	for _, v := range values {
		typ := reflect.TypeOf(v)
		for i := range typ.NumField() {
			names = append(names, typ.Field(i).Name)
		}
	}
	return names
}

func recoverText(f func()) (text string) {
	defer func() {
		if r := recover(); r != nil {
			text, _ = r.(string)
		}
	}()
	f()
	return ""
}
