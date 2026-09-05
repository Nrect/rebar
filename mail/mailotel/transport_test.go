package mailotel_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailotel"
)

func TestSend_ClassifiesOutcome(t *testing.T) {
	t.Parallel()
	rejected := &mail.RejectedError{Code: "MessageRejected", Reason: "on the suppression list"}

	cases := map[string]struct {
		err  error
		want mailotel.Result
	}{
		"успех":              {nil, mailotel.ResultOK},
		"постоянный отказ":   {rejected, mailotel.ResultRejected},
		"отказ под обёрткой": {fmt.Errorf("sesv2: %w", rejected), mailotel.ResultRejected},
		"сетевой сбой":       {io.ErrUnexpectedEOF, mailotel.ResultError},
		"таймаут":            {context.DeadlineExceeded, mailotel.ResultError},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reader, meter := newMeter(t)
			tr, err := mailotel.Wrap(newStub(tc.err), meter)
			require.NoError(t, err)

			_, sendErr := tr.Send(context.Background(), envelope("verify"))
			if tc.err == nil {
				require.NoError(t, sendErr)
			} else {
				require.ErrorIs(t, sendErr, tc.err)
			}

			m := metricByName(t, collect(t, reader), counterName)
			assert.Equal(t, unitEmail, m.Unit)
			assert.Equal(t, 1, counterPoints(t, m), "ровно одна пара меток")
			assert.Equal(t, int64(1), counterValue(t, m, "verify", tc.want))
		})
	}
}

// Метки разделяют попытки по kind и исходу: иначе доля rejected, на которой
// стоит алерт «провайдер отказывает», считалась бы по всей почте разом.
func TestSend_CountsPerKindAndResult(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	stub := newStub(nil)
	tr, err := mailotel.Wrap(stub, meter)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = tr.Send(ctx, envelope("verify"))
	require.NoError(t, err)
	_, err = tr.Send(ctx, envelope("verify"))
	require.NoError(t, err)
	_, err = tr.Send(ctx, envelope("reset"))
	require.NoError(t, err)

	stub.err = &mail.RejectedError{Reason: "mailbox unavailable"}
	_, err = tr.Send(ctx, envelope("verify"))
	require.Error(t, err)

	m := metricByName(t, collect(t, reader), counterName)
	assert.Equal(t, 3, counterPoints(t, m))
	assert.Equal(t, int64(2), counterValue(t, m, "verify", mailotel.ResultOK))
	assert.Equal(t, int64(1), counterValue(t, m, "reset", mailotel.ResultOK))
	assert.Equal(t, int64(1), counterValue(t, m, "verify", mailotel.ResultRejected))
	assert.Equal(t, int64(0), counterValue(t, m, "verify", mailotel.ResultError))
}

// Deliver ветвится по *RejectedError и по sentinel'ам: декоратор, потерявший
// обёртку, превратил бы постоянный отказ в вечные ретраи.
func TestSend_PassesResultAndErrorThrough(t *testing.T) {
	t.Parallel()
	_, meter := newMeter(t)
	rejected := &mail.RejectedError{Code: "MessageRejected", Reason: "on the suppression list"}
	stub := newStub(fmt.Errorf("sesv2: %w", rejected))
	stub.res = mail.SendResult{ProviderMessageID: "0100-abc"}
	tr, err := mailotel.Wrap(stub, meter)
	require.NoError(t, err)

	env := envelope("verify")
	res, sendErr := tr.Send(context.Background(), env)

	assert.Equal(t, "0100-abc", res.ProviderMessageID, "SendResult next как есть")
	assert.Equal(t, env, stub.last, "конверт дошёл до next без правок")
	require.ErrorIs(t, sendErr, rejected)
	assert.True(t, mail.IsRejected(sendErr))

	var got *mail.RejectedError
	require.ErrorAs(t, sendErr, &got)
	assert.Equal(t, "MessageRejected", got.Code)
}

// Sentinel ядра тоже обязан пережить декоратор.
func TestSend_KeepsSentinelError(t *testing.T) {
	t.Parallel()
	_, meter := newMeter(t)
	tr, err := mailotel.Wrap(newStub(fmt.Errorf("stub: %w", mail.ErrUnavailable)), meter)
	require.NoError(t, err)

	_, sendErr := tr.Send(context.Background(), envelope("verify"))
	require.ErrorIs(t, sendErr, mail.ErrUnavailable)
	assert.False(t, mail.IsRejected(sendErr))
}

func TestName_IsPassedThrough(t *testing.T) {
	t.Parallel()
	_, meter := newMeter(t)
	stub := newStub(nil)
	stub.name = "sesv2"
	tr, err := mailotel.Wrap(stub, meter)
	require.NoError(t, err)

	assert.Equal(t, mail.TransportName("sesv2"), tr.Name())
}

// Шаг 0 Deliver узнаёт Unconfigured по имени; декоратор, подменивший его,
// сжёг бы попытки прода без провайдера на failed(exhausted).
func TestWrap_UnconfiguredKeepsNameAndStaysTemporary(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	tr, err := mailotel.Wrap(mail.Unconfigured{}, meter)
	require.NoError(t, err)

	assert.Equal(t, mail.UnconfiguredName, tr.Name())

	_, sendErr := tr.Send(context.Background(), envelope("verify"))
	require.ErrorIs(t, sendErr, mail.ErrTransportUnconfigured)
	assert.False(t, mail.IsRejected(sendErr), "письмо обязано остаться в очереди")

	m := metricByName(t, collect(t, reader), counterName)
	assert.Equal(t, int64(1), counterValue(t, m, "verify", mailotel.ResultError))
}

// Декоратор подставляется вместо порта, а не рядом с ним.
func TestTransport_ImplementsPort(t *testing.T) {
	t.Parallel()
	_, meter := newMeter(t)
	tr, err := mailotel.Wrap(newStub(errors.New("boom")), meter)
	require.NoError(t, err)

	var port mail.Transport = tr
	assert.Equal(t, mail.TransportName("stub"), port.Name())
}

// Таймаут отправки — главный источник result=error, и ctx к моменту записи уже
// отменён: попытка обязана попасть в счётчик всё равно, иначе алерт «провайдер
// недоступен» слеп ровно там, где нужен.
func TestSend_CountsUnderCanceledContext(t *testing.T) {
	t.Parallel()
	reader, meter := newMeter(t)
	tr, err := mailotel.Wrap(newStub(context.DeadlineExceeded), meter)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, sendErr := tr.Send(ctx, envelope("verify"))
	require.ErrorIs(t, sendErr, context.DeadlineExceeded)

	m := metricByName(t, collect(t, reader), counterName)
	assert.Equal(t, int64(1), counterValue(t, m, "verify", mailotel.ResultError))
}
