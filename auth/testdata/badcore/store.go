// Корпус стража импортов: заведомо запрещённый в ядре импорт драйвера.
// Каталог testdata компилятором не собирается — файл нужен только тесту
// TestImportGuard_Fires, который обязан на нём сработать.
package badcore

import "github.com/jackc/pgx/v5"

var _ = pgx.ErrNoRows
