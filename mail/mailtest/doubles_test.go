package mailtest_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailtest"
)

// errUnavailable — сбой контрагента, который тест внедряет полем Err.
var errUnavailable = errors.New("mailtest_test: backend is down")

func TestTransport_RecordsSentEnvelopes(t *testing.T) {
	t.Parallel()
	transport := mailtest.NewTransport()
	env := envelope("verify:a", storeBase)

	assert.Equal(t, mailtest.TransportName, transport.Name())
	res, err := transport.Send(context.Background(), env)
	require.NoError(t, err)
	assert.NotEmpty(t, res.ProviderMessageID)

	sent := transport.Sent()
	require.Len(t, sent, 1)
	assert.Equal(t, env.ID, sent[0].ID)
	assert.Equal(t, "Подтверждение почты", sent[0].Subject)

	sent[0].Headers["X-Trace"] = "подмена"
	assert.Equal(t, "verify:a", transport.Sent()[0].Headers["X-Trace"], "Sent отдаёт копии")
}

// RejectFor — постоянный отказ, FailFor — временный сбой: Deliver обязан
// различать их, поэтому и двойник различает.
func TestTransport_RejectAndFailAreDistinct(t *testing.T) {
	t.Parallel()
	transport := mailtest.NewTransport()
	rejected := envelope("verify:a", storeBase)
	transport.RejectFor[rejected.To.Email] = "MessageRejected"
	flaky := envelope("verify:b", storeBase)
	transport.FailFor[flaky.To.Email] = 2

	_, err := transport.Send(context.Background(), rejected)
	require.Error(t, err)
	assert.True(t, mail.IsRejected(err))

	for range 2 {
		_, err = transport.Send(context.Background(), flaky)
		require.ErrorIs(t, err, mailtest.ErrSendFailed)
		assert.False(t, mail.IsRejected(err), "временный сбой — не отказ")
	}
	_, err = transport.Send(context.Background(), flaky)
	require.NoError(t, err, "счётчик сбоев исчерпан")
	assert.Len(t, transport.Sent(), 1, "отказавшие письма не записаны")
}

func TestTransport_SendHookOverrides(t *testing.T) {
	t.Parallel()
	transport := mailtest.NewTransport()
	env := envelope("verify:a", storeBase)
	transport.RejectFor[env.To.Email] = "MessageRejected"
	transport.SendHook = func(ctx context.Context, _ mail.Envelope) (mail.SendResult, error) {
		<-ctx.Done()
		return mail.SendResult{}, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := transport.Send(ctx, env)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, transport.Sent())
}

func TestMemSuppressor_StoresAndFails(t *testing.T) {
	t.Parallel()
	supp := mailtest.NewMemSuppressor()
	ctx := context.Background()

	_, found, err := supp.IsSuppressed(ctx, "teacher@school.ru")
	require.NoError(t, err)
	assert.False(t, found)

	sup := mail.Suppression{Email: "teacher@school.ru", Reason: mail.SuppressHardBounce, At: storeBase}
	require.NoError(t, supp.Suppress(ctx, sup))
	got, found, err := supp.IsSuppressed(ctx, "teacher@school.ru")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, sup, got)
	assert.Equal(t, []mail.Suppression{sup}, supp.Suppressions())

	supp.Err = errUnavailable
	_, _, err = supp.IsSuppressed(ctx, "teacher@school.ru")
	require.ErrorIs(t, err, errUnavailable)
	require.ErrorIs(t, supp.Suppress(ctx, sup), errUnavailable)
}

// Двойники живут под -race: несколько воркеров кладут, забирают и завершают
// строки одновременно, и ни одна не уходит в транспорт дважды.
func TestDoubles_AreRaceSafe(t *testing.T) {
	t.Parallel()
	const workers, perWorker = 4, 25

	store := mailtest.NewMemStore()
	transport := mailtest.NewTransport()
	ctx := context.Background()

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWorker {
				key := fmt.Sprintf("verify:%d-%d", w, i)
				if _, err := store.Enqueue(ctx, envelope(key, storeBase)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				claimed, err := store.Claim(ctx, storeBase, storeLease, 7)
				if err != nil {
					t.Error(err)
					return
				}
				if len(claimed) == 0 {
					return
				}
				for _, env := range claimed {
					if _, err = transport.Send(ctx, env); err != nil {
						t.Error(err)
						return
					}
					// Не require: горутина не вправе звать FailNow.
					if err = store.Finish(ctx, mail.FinishRequest{
						ID: env.ID, Outcome: mail.FinishSent, Now: storeBase,
						Transport: mailtest.TransportName,
					}); err != nil {
						t.Error(err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()

	assert.Len(t, transport.Sent(), workers*perWorker, "каждая строка ушла один раз")
	stats, err := store.Stats(ctx, storeBase)
	require.NoError(t, err)
	assert.Equal(t, int64(0), stats.Pending)
}
