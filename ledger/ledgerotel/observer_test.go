package ledgerotel_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nrect/rebar/kit/secrets"
	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/ledger/ledgerotel"
	"github.com/nrect/rebar/ledger/ledgertest"
)

// Имя и единица — контракт для алертов потребителя: тест держит их литералами.
const (
	mismatchName = "ledger_reconcile_mismatch"
	unitMismatch = "{mismatch}"
)

func TestNewObserver_PanicsOnNilMeter(t *testing.T) {
	t.Parallel()
	assert.PanicsWithValue(t, "ledgerotel.NewObserver: nil meter", func() { _, _ = ledgerotel.NewObserver(nil) })
}

// Отказ метра возвращается ошибкой, а не роняет процесс.
func TestNewObserver_ReturnsInstrumentError(t *testing.T) {
	t.Parallel()
	var (
		obs ledger.Observer
		err error
	)
	assert.NotPanics(t, func() { obs, err = ledgerotel.NewObserver(failingMeter{}) })
	require.ErrorIs(t, err, errMeter)
	assert.Nil(t, obs)
}

// Все пары книги × AllChecks есть в scrape с нуля до первого расхождения:
// первое же расхождение видно increase().
func TestObserver_WatchStartsEverySeriesAtZero(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	obs, err := ledgerotel.NewObserver(meter)
	require.NoError(t, err)

	obs.Watch("wallet")
	obs.Watch("points")

	points := mismatchPoints(t, collect(t, reader))
	assert.Len(t, points, 2*len(ledger.AllChecks))
	for _, dp := range points {
		assert.Zero(t, dp.Value)
	}
	assertClosedLabels(t, points, "wallet", "points")
}

// Находка прибавляет по единице на расхождение своей проверке и новых рядов не
// рождает: метка вне закрытых наборов не появляется.
func TestObserver_EveryPointIsFromClosedSets(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	obs, err := ledgerotel.NewObserver(meter)
	require.NoError(t, err)
	obs.Watch("wallet")

	mismatches := make([]ledger.Mismatch, 0, len(ledger.AllChecks))
	for _, check := range ledger.AllChecks {
		mismatches = append(mismatches, ledger.Mismatch{EntryID: uuid.New(), Seq: 7, Check: check})
	}
	obs.Found(t.Context(), ledger.Finding{Book: "wallet", Account: uuid.New(), Mismatches: mismatches})
	obs.Found(t.Context(), ledger.Finding{Book: "wallet", Account: uuid.New(), Mismatches: []ledger.Mismatch{
		{Seq: 3, Check: ledger.CheckHeadBalance},
	}})

	points := mismatchPoints(t, collect(t, reader))
	assert.Len(t, points, len(ledger.AllChecks), "находка рядов не добавляет")
	assertClosedLabels(t, points, "wallet")
	for _, check := range ledger.AllChecks {
		want := int64(1)
		if check == ledger.CheckHeadBalance {
			want = 2
		}
		assert.Equal(t, want, mismatchCount(t, points, "wallet", check), string(check))
	}
}

// Прогон, оборванный отменой, отдаёт находку с отменённым ctx: она обязана
// попасть в счётчик всё равно.
func TestObserver_CountsUnderCanceledContext(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	obs, err := ledgerotel.NewObserver(meter)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	obs.Found(ctx, ledger.Finding{Book: "wallet", Mismatches: []ledger.Mismatch{{Seq: 1, Check: ledger.CheckSignature}}})

	assert.Equal(t, int64(1), mismatchCount(t, mismatchPoints(t, collect(t, reader)), "wallet", ledger.CheckSignature),
		"счётчик под отменённым контекстом")
}

// Классификация метрики совпадает с тем, что нашла настоящая сверка (PATTERNS
// §8): чужая подпись и запись под ключом, которого нет в Config, считаются
// ровно своими проверками.
func TestObserver_MatchesReconciler(t *testing.T) {
	t.Parallel()
	book := ledger.Book{Name: "wallet", Unit: "RUB", Kinds: []ledger.KindSpec{
		{Name: "topup", Sign: ledger.SignCredit, Reference: ledger.Optional, Attribution: ledger.Optional},
	}}
	store := ledgertest.NewMemStore(book)
	own, foreign, rotated := key(t), key(t), key(t)
	post := func(keys map[secrets.KeyID][]byte, active secrets.KeyID, account uuid.UUID, idem string) {
		svc := ledger.NewService(store, ledger.Config{Book: book, Keys: keys, ActiveKey: active})
		_, err := svc.Post(t.Context(), ledger.PostRequest{Account: account, Kind: "topup", AmountMinor: 100, IdempotencyKey: idem})
		require.NoError(t, err)
	}
	forged, unknown := uuid.New(), uuid.New()
	post(map[secrets.KeyID][]byte{1: own}, 1, forged, "own")
	post(map[secrets.KeyID][]byte{1: foreign}, 1, forged, "forged")
	post(map[secrets.KeyID][]byte{2: rotated}, 2, unknown, "rotated")

	reader, meter := newMeter(t)
	metered, err := ledgerotel.NewObserver(meter)
	require.NoError(t, err)
	recorded := ledgertest.NewObserver()
	svc := ledger.NewService(store, ledger.Config{Book: book, Keys: map[secrets.KeyID][]byte{1: own}, ActiveKey: 1})
	_, err = ledger.NewReconciler(svc, fanOut{metered, recorded}, ledger.ReconcileConfig{Accounts: 10, Page: 10}).Run(t.Context())
	require.NoError(t, err)

	want := map[ledger.Check]int64{}
	for _, f := range recorded.Findings() {
		for _, m := range f.Mismatches {
			want[m.Check]++
		}
	}
	require.Equal(t, map[ledger.Check]int64{ledger.CheckSignature: 1, ledger.CheckUnknownKey: 1}, want,
		"контроль: сверка нашла ровно чужую подпись и неизвестный ключ")
	points := mismatchPoints(t, collect(t, reader))
	for _, check := range ledger.AllChecks {
		assert.Equal(t, want[check], mismatchCount(t, points, "wallet", check), string(check))
	}
}

// fanOut — метрика и запись для утверждения одним наблюдателем.
type fanOut []ledger.Observer

func (o fanOut) Watch(book string) {
	for _, x := range o {
		x.Watch(book)
	}
}

func (o fanOut) Found(ctx context.Context, f ledger.Finding) {
	for _, x := range o {
		x.Found(ctx, f)
	}
}

func key(t *testing.T) []byte {
	t.Helper()
	k, err := secrets.GenerateKey()
	require.NoError(t, err)
	return k
}

func newMeter(t *testing.T) (*sdkmetric.ManualReader, metric.Meter) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, mp.Shutdown(context.Background())) })
	return reader, mp.Meter("rebar.ledger")
}

// collect — метрики одного scrape. Пустой scrape — nil, а не отказ помощника:
// нет рядов — называет утверждение теста.
func collect(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	if len(rm.ScopeMetrics) == 0 {
		return nil
	}
	require.Len(t, rm.ScopeMetrics, 1)
	return rm.ScopeMetrics[0].Metrics
}

// mismatchPoints — точки счётчика расхождений; nil — scrape пуст. Scrape без
// инструмента — отказ: имя, единица и монотонность — контракт.
func mismatchPoints(t *testing.T, ms []metricdata.Metrics) []metricdata.DataPoint[int64] {
	t.Helper()
	if len(ms) == 0 {
		return nil
	}
	for _, m := range ms {
		if m.Name != mismatchName {
			continue
		}
		require.Equal(t, unitMismatch, m.Unit)
		sum, ok := m.Data.(metricdata.Sum[int64])
		require.True(t, ok, "%s обязан быть Int64Counter", mismatchName)
		require.True(t, sum.IsMonotonic, "%s обязан быть счётчиком, а не UpDown", mismatchName)
		return sum.DataPoints
	}
	require.Fail(t, "инструмента нет в scrape", mismatchName)
	return nil
}

// mismatchCount — значение ряда книги и проверки; ноль, если ряда нет.
func mismatchCount(t *testing.T, points []metricdata.DataPoint[int64], book string, check ledger.Check) int64 {
	t.Helper()
	want := attribute.NewSet(attribute.String("book", book), attribute.String("check", string(check)))
	for _, dp := range points {
		if dp.Attributes.Equals(&want) {
			return dp.Value
		}
	}
	return 0
}

// assertClosedLabels — у каждой точки ровно книга и проверка, обе из закрытых наборов.
func assertClosedLabels(t *testing.T, points []metricdata.DataPoint[int64], books ...string) {
	t.Helper()
	for _, dp := range points {
		require.Equal(t, 2, dp.Attributes.Len(), "меток ровно две: book и check")
		book, ok := dp.Attributes.Value("book")
		require.True(t, ok, "у точки нет метки book")
		check, ok := dp.Attributes.Value("check")
		require.True(t, ok, "у точки нет метки check")
		assert.Contains(t, books, book.AsString())
		assert.Contains(t, ledger.AllChecks, ledger.Check(check.AsString()), "метка check вне ledger.AllChecks")
	}
}

var errMeter = errors.New("meter: instrument refused")

// failingMeter — метр, отказывающий в счётчике: ошибка сборки возвращается.
type failingMeter struct{ noop.Meter }

func (failingMeter) Int64Counter(string, ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return nil, errMeter
}
