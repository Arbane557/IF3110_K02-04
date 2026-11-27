# Panduan test

Aturan setiap fitur yang di-push ada di [testing_req.md](testing_req.md).

Test berada di package fitur masing-masing: unit pada `*_test.go`, integration pada `integration_test.go`. Pakai pola setup -> aksi -> assertion dan tambahkan case ke file yang sudah ada. Test tidak perlu meniru seluruh struktur service atau membuat framework sendiri.

Unit berjalan tanpa PostgreSQL. Integration memakai PostgreSQL nyata, tag `integration`, dan `internal/testutil.NewDatabase(t, platform)`: `true` memuat SQL init aplikasi; `false` menyediakan target kosong. Helper membuat schema unik dan membersihkannya setelah test. Gunakan database test disposable, bukan database aplikasi/production.

Dari `src/backend`:

```powershell
go test -count=1 ./...
go vet ./...
$env:INTEGRATION_DATABASE_URL = 'postgres://app:secret@localhost:55433/wbd_test?sslmode=disable'
go test -tags=integration -count=1 -timeout=120s ./...
```

Sediakan PostgreSQL test terlebih dahulu; untuk test registry, gunakan URL TCP dengan port eksplisit dan credential yang valid. CI sudah menyediakan PostgreSQL 17. Race detector Windows memerlukan GCC; CI Linux menjalankannya.

Saat mengubah fitur, tambahkan unit untuk logika terkait dan integration untuk interaksi nyatanya. Periksa hasil dan state tersimpan, bukan hanya status HTTP. Jangan mock authorizer lalu mengklaim aturan permission PostgreSQL sudah teruji. Untuk rollback, perubahan valid harus terjadi sebelum statement gagal.

Test draft update menemukan bug NULL `error_message`; update sekarang memakai scanner nullable yang sama dengan Get/List. Regression-nya ada di migration integration test.

Masalah kode yang masih layak diperiksa:

- `auth/access.go`: admin otomatis melewati permission database; action tidak dikenal dianggap view.
- Auth session dibaca ulang oleh middleware dan authorizer dalam request yang sama.
- `migration/service.go`: urutan eksekusi version belum dipaksa; SQL bebas dan EXPLAIN perlu batasan yang jelas.
- `database/connection.go`: mutex global ditahan saat membuka koneksi target.
- `database/catalog.go`: pagination tanpa ORDER BY; error lookup bisa terlihat seperti identifier salah.
- `migration/handler.go`: decoder belum menolak trailing JSON seperti auth/registry.

Coverage belum lengkap: concurrency, seluruh kombinasi CRUD/error dependency, pencabutan membership/access, dan seluruh verb migration dengan permission nyata masih perlu dikembangkan.
