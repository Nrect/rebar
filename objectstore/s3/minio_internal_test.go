package s3

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// minioImage — пин по digest: тег переезжает на новый образ, и «тот же тест на
// той же версии» перестаёт быть правдой (как postgres в pgtest).
const minioImage = "minio/minio:RELEASE.2025-09-07T16-13-09Z@sha256:" +
	"14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e"

const (
	minioUser     = "objectstore"
	minioPassword = "objectstore-secret"
	minioBucket   = "catalog"
)

// minioEndpoint — адрес поднятого MinIO; пуст, когда тесты идут с -short.
var minioEndpoint string

func TestMain(m *testing.M) {
	flag.Parse() // testing.Short() до m.Run требует разобранных флагов
	if testing.Short() {
		os.Exit(m.Run()) // интеграционные тесты пропустят себя сами
	}
	ctx := context.Background()
	ctr, endpoint, err := startMinIO(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "старт MinIO:", err)
		os.Exit(1)
	}
	minioEndpoint = endpoint
	if err = putBucket(ctx, endpoint, minioBucket); err != nil {
		fmt.Fprintln(os.Stderr, "создание бакета:", err)
		_ = ctr.Terminate(context.Background())
		os.Exit(1)
	}
	code := m.Run()
	_ = ctr.Terminate(context.Background())
	os.Exit(code)
}

func startMinIO(ctx context.Context) (testcontainers.Container, string, error) {
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        minioImage,
			Cmd:          []string{"server", "/data"},
			ExposedPorts: []string{"9000/tcp"},
			Env: map[string]string{
				"MINIO_ROOT_USER":     minioUser,
				"MINIO_ROOT_PASSWORD": minioPassword,
			},
			WaitingFor: wait.ForHTTP("/minio/health/live").
				WithPort("9000/tcp").
				WithStartupTimeout(120 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		return nil, "", err
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		return nil, "", err
	}
	port, err := ctr.MappedPort(ctx, "9000/tcp")
	if err != nil {
		return nil, "", err
	}
	return ctr, "http://" + host + ":" + port.Port(), nil
}

// putBucket кладёт бакет тем же подписывальщиком, каким работает адаптер:
// первый настоящий ответ MinIO на нашу подпись приходит уже здесь.
func putBucket(ctx context.Context, endpoint, bucket string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint+"/"+bucket, http.NoBody)
	if err != nil {
		return err
	}
	s := signer{accessKeyID: minioUser, secret: minioPassword, region: "us-east-1", service: service}
	req.Header.Set(headerContentSHA256, emptyHash)
	s.sign(req, emptyHash, time.Now().UTC())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("MinIO ответил %d на создание бакета: подпись не принята", resp.StatusCode)
	}
	return nil
}
