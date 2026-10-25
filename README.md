# Database Management Platform

Platform web untuk mengelola koneksi PostgreSQL target, mengatur akses berbasis group, melihat schema/data, dan menjalankan migration secara terkontrol. Proyek ini dibuat untuk Tugas Besar IF3110 Milestone 1.

## Fitur

- Login dengan username/password dan session berbasis cookie.
- Platform Admin mengelola user, group, koneksi database target, dan permission group.
- Akses database diberikan melalui group dengan level **View Only** atau **View + Edit**.
- Registry untuk satu atau lebih database PostgreSQL target.
- Database viewer untuk schema, table, column, constraint, index, dan data tabel.
- Migration berurutan dengan transaction, commit/rollback, dan history per database.
- Preview execution plan migration menggunakan PostgreSQL `EXPLAIN` sebelum eksekusi.
- Unit test, integration test PostgreSQL, dan GitHub Actions CI.

## Teknologi

| Komponen | Teknologi |
| --- | --- |
| Backend | Go 1.22 |
| HTTP | `net/http.ServeMux` |
| PostgreSQL client | `pgx/v5` dan `pgxpool` |
| Frontend | Go template dan JavaScript ringan |
| Container lokal | Docker Compose dan PostgreSQL 17 |
| CI | GitHub Actions |

## Struktur proyek

```text
src/backend/
|-- auth/          # login, session, CSRF, user, group, authorization
|-- database/      # registry, pgxpool, catalog/schema viewer
|-- migration/     # draft, execute, history migration
|-- db/init/       # inisialisasi schema PostgreSQL internal
|-- templates/     # halaman HTML
|-- static/        # stylesheet
|-- main.go        # bootstrap aplikasi
`-- routes.go      # registrasi HTTP routes

.github/workflows/ # GitHub Actions CI
docker-compose.yml # environment lokal
```

## Menjalankan secara lokal

### Prasyarat

- Docker Desktop / Docker Engine dengan Docker Compose.
- Atau Go 1.22 dan PostgreSQL 17 bila menjalankan backend tanpa container.

### Dengan Docker Compose

```bash
docker compose up --build
```

Buka aplikasi pada [http://localhost:6969](http://localhost:6969).

| Service | Fungsi | Port host |
| --- | --- | --- |
| `backend` | Aplikasi Go | `6969` |
| `database` | Database internal platform | `55432` |
| `target-db-1` | Contoh PostgreSQL target | `5433` |

Database internal diinisialisasi dari `src/backend/db/init/`. Script init PostgreSQL hanya dijalankan saat volume baru dibuat. Untuk memulai ulang data development dari nol, gunakan perintah berikut dengan hati-hati karena menghapus seluruh data volume lokal:

```bash
docker compose down -v
docker compose up --build
```

### Environment variable

| Variable | Keterangan |
| --- | --- |
| `DATABASE_URL` | Koneksi ke database internal platform. |
| `CREDENTIAL_ENCRYPTION_KEY` | Key AES-GCM 32-byte dalam 64 karakter heksadesimal untuk mengenkripsi password PostgreSQL target. |
| `APP_ENV` | Set `production` untuk menandai cookie sebagai `Secure`; mode lokal dapat dibiarkan kosong. |

Jangan memakai `CREDENTIAL_ENCRYPTION_KEY` development sebagai secret production. Simpan key production di secret manager atau environment deployment, bukan repository.

## Alur penggunaan

### Sebagai Platform Admin

1. Login sebagai Platform Admin.
2. Buka halaman **Databases** dan daftarkan PostgreSQL target: host, port, nama database, username PostgreSQL, password PostgreSQL, dan SSL mode.
3. Buat group di halaman **Groups**.
4. Tambahkan user ke group.
5. Berikan group akses pada database target:
   - `view`: melihat schema, data, dan history migration.
   - `edit`: seluruh hak `view`, serta membuat, mengubah, dan menjalankan migration.

Password PostgreSQL target adalah credential teknis yang dipakai aplikasi untuk membuat koneksi ke target. User platform tidak memasukkan atau melihat password target tersebut.

### Sebagai user database

1. Login ke aplikasi.
2. Buka **Databases**.
3. Klik **Browse schema** pada database yang tersedia untuk group user.
4. Pilih schema, lalu tabel.
5. Viewer menampilkan kolom, default value, nullable status, constraint, index, serta data tabel dengan pagination.

## Access control

Permission selalu diperiksa di backend, bukan hanya melalui tampilan frontend.

| Aksi | Platform Admin | View Only | View + Edit |
| --- | --- | --- | --- |
| Kelola user/group/registry koneksi | Ya | Tidak | Tidak |
| Lihat schema dan data | Melalui authorization database | Ya | Ya |
| Lihat migration history | Melalui authorization database | Ya | Ya |
| Buat/ubah/jalankan migration | Melalui authorization database | Tidak | Ya |
| Preview execution plan migration | Melalui authorization database | Tidak | Ya |

### Aturan akses efektif multi-group

Seorang user dapat berada di banyak group. Hak efektif untuk sebuah database adalah hak tertinggi dari group yang dimilikinya:

```text
edit > view > tidak ada akses
```

Contoh: bila Group A memberi `view` dan Group B memberi `edit` pada database yang sama, user memperoleh akses `edit`.

> Catatan implementasi saat ini: `AccessService` masih memberi Platform Admin izin otomatis ke semua database target. Ini perlu diselaraskan lebih lanjut dengan ketentuan bahwa Platform Admin bukan tingkat akses database.

## Session dan keamanan

| Mekanisme | Implementasi |
| --- | --- |
| Password user | bcrypt, tidak disimpan plaintext |
| Session identifier | 32 random byte, dikirim sebagai opaque cookie dan disimpan sebagai SHA-256 hash |
| Idle timeout | 30 menit tanpa aktivitas |
| Absolute expiration | 24 jam sejak login; tidak diperpanjang oleh rotasi |
| Rotasi session | Saat login, saat privilege meningkat, dan setiap 15 menit pemakaian aktif |
| Grace period token lama | 30 detik setelah rotasi untuk request bersamaan/in-flight |
| Logout/revocation | Session dinonaktifkan di backend |
| Session cookie | `HttpOnly`, `SameSite=Lax`, dan `Secure` saat HTTPS/`APP_ENV=production` |
| CSRF | Cookie token + header `X-CSRF-Token` atau field form `_csrf` untuk request perubahan state |
| Credential PostgreSQL target | Dienkripsi AES-GCM saat disimpan dan tidak dikirim pada API response |

Database viewer hanya menjalankan query baca. Nama schema/table diverifikasi terhadap PostgreSQL target sebelum dipakai sebagai identifier query. Nilai query memakai parameter SQL, dan data tabel dibatasi maksimal 100 row per request.

## API utama

Seluruh endpoint API memerlukan session yang valid. Endpoint yang mengubah state juga memerlukan CSRF token.

```text
GET    /api/databases
POST   /api/databases
GET    /api/databases/{databaseID}/schemas
GET    /api/databases/{databaseID}/schemas/{schema}/tables
GET    /api/databases/{databaseID}/schemas/{schema}/tables/{table}
GET    /api/databases/{databaseID}/schemas/{schema}/tables/{table}/rows?page=1&pageSize=50

GET    /api/databases/{databaseID}/migrations
POST   /api/databases/{databaseID}/migrations
PUT    /api/databases/{databaseID}/migrations/{version}
POST   /api/databases/{databaseID}/migrations/explain
POST   /api/databases/{databaseID}/migrations/{version}/execute
```

## Bonus: Query execution plan analysis

Sebelum migration dieksekusi, user dengan akses `View + Edit` dapat menekan **Preview plan** pada halaman Migration. Aplikasi menjalankan PostgreSQL `EXPLAIN` terhadap SQL draft dan menampilkan output plan tanpa menjalankan SQL migration tersebut.

Endpoint berikut memerlukan session, permission `database:edit`, dan CSRF token:

```text
POST /api/databases/{databaseID}/migrations/explain
```

Request body:

```json
{"sql":"CREATE INDEX users_email_idx ON users(email)"}
```

Implementasi saat ini memakai `EXPLAIN`, sehingga menampilkan rencana dan estimasi PostgreSQL. `EXPLAIN ANALYZE` belum digunakan karena menjalankan query sungguhan dan dapat mengubah data bila SQL draft bersifat mutatif.

## Testing

Jalankan seluruh test:

```bash
cd src/backend
go test ./...
```

Untuk menjalankan integration test PostgreSQL secara lokal, sediakan database test lalu atur URL-nya:

```powershell
$env:INTEGRATION_DATABASE_URL = "postgres://app:secret@localhost:55432/app?sslmode=disable"
cd src/backend
go test ./...
```

Test mencakup validasi registry, enkripsi credential, handler authorization, connection manager, catalog schema/table/constraint/index/data, dan lifecycle migration.

## Continuous Integration

Workflow [CI](.github/workflows/ci.yml) berjalan pada setiap `push` dan `pull_request`:

1. Menjalankan unit test dengan `go test ./...`.
2. Menjalankan PostgreSQL 17 service container.
3. Menjalankan integration test dengan `INTEGRATION_DATABASE_URL`.

## Batasan

- Hanya PostgreSQL yang didukung.
- Schema diff dan migration generation otomatis adalah fitur bonus yang belum diimplementasikan.
- Preview plan memakai `EXPLAIN`; `EXPLAIN ANALYZE` belum digunakan karena dapat mengeksekusi SQL migration.
- Viewer bersifat read-only; perubahan database dilakukan melalui modul Migration.
