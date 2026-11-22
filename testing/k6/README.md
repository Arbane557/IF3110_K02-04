# Stress Testing Migration Platform dengan k6

## Tujuan

Pengujian ini menaikkan beban secara bertahap di atas beban normal, menahan beban puncak,
kemudian menurunkan beban untuk mengamati pemulihan. Setiap VU mengakses API dan
mengirim job migration pada satu dari daftar database yang dikonfigurasi, sehingga request
meliputi database yang sama dan berbeda.

## Prasyarat

1. Jalankan stack dengan Docker Compose.
2. Siapkan akun dengan hak `edit`, migration berstatus `draft`, dan group approval bila
   jalur approval aktif.
3. Migration yang dipakai untuk submit harus sudah disetujui sebelum test. Pastikan status
   job tidak lagi `pending_review` sebelum menjalankan k6.
4. Instal k6 dan verifikasi bahwa worker aktif.

## Menjalankan

Contoh Windows PowerShell:

```powershell
$env:BASE_URL = "http://localhost:6969"
$env:K6_USERNAME = "admin"
$env:K6_PASSWORD = "admin1234"
$env:DATABASE_IDS = "1,1,2"
$env:MIGRATION_VERSIONS = "1,2,1"
$env:NORMAL_VUS = "5"
k6 run testing/k6/stress-migration.js
```

`DATABASE_IDS` dan `MIGRATION_VERSIONS` dipasangkan berdasarkan posisi. Jadi contoh di
atas menguji kombinasi `(1,1)`, `(1,2)`, dan `(2,1)`. Substitusi dengan ID database dan
versi milik lingkungan test.

Tahapan default adalah normal 5 VU, 2x normal, 4x normal, kembali ke normal, lalu turun ke 0.
Tim dapat mengganti `NORMAL_VUS` dengan beban normal yang disepakati dan mengubah durasi
lewat `RAMP_UP`, `NORMAL_DURATION`, `STRESS_DURATION`, `PEAK_DURATION`,
`COOLDOWN_DURATION`, dan `RECOVERY_DURATION`.

## Metrik dan threshold

- Response time API: built-in `http_req_duration` dan `api_duration_ms`; dilaporkan sebagai p50/p95.
- Throughput API/job: `http_reqs.rate` dan `migration_jobs_success` (job sukses per detik).
- Error rate: built-in `http_req_failed` serta `api_error_rate`.
- Queue wait: waktu dari penerimaan HTTP 202 sampai job meninggalkan status `pending_review`/`pending`.
- Job duration: waktu dari submission sampai status terminal `success`, `failed`, atau `cancelled`.
- Job throughput: counter `migration_jobs_success` dibagi durasi total test.

HTTP 202 hanya menandakan job diterima; script tidak menghitung submission sebagai sukses
eksekusi. Pengiriman hanya dihitung sebagai sukses bila status akhir job adalah `success`.

Threshold default:

```text
p95 API < 500 ms
API error rate < 5%
```

Atur threshold lewat `API_P95_THRESHOLD_MS` dan `API_ERROR_THRESHOLD`.
`api_duration_ms` dihitung dari durasi request API, sedangkan `api_error_rate` menandai
response selain 2xx sebagai error. HTTP 409 atau 423 tetap dihitung sebagai error API.

## Penjelasan singkat script

Script `stress-migration.js` melakukan login VU dan menyimpan session cookie serta CSRF
token untuk request perubahan. Setiap iterasi menguji:

1. `GET` daftar migration dan daftar job.
2. `POST` enqueue migration pada satu pasangan database dan versi.
3. Polling `GET` job sampai `success`, `failed`, `cancelled`, atau timeout.

Hasil submission HTTP 202 tidak dihitung sebagai eksekusi berhasil; hanya status job
`success` yang menaikkan counter job sukses. Script menggunakan staged VU ramp untuk naik
di atas beban normal, mencapai puncak, lalu turun kembali ke 0 untuk mengamati pemulihan.

## Hasil k6 testing

Pengujian dijalankan pada 22 November 2026 dengan backend, worker, RabbitMQ, PostgreSQL
internal, dan PostgreSQL target aktif. Pengujian menggunakan 12 migration unik pada database
registry 2 dan 3. Ringkasan dari `testing/k6/stress-migration-summary.json`:

| Metrik | Hasil |
|---|---:|
| Durasi | 95 detik |
| API p50 | 9.51 ms |
| API p95 | 19.72 ms |
| Throughput API | 14.97 request/detik |
| API error rate | 2.85% |
| Job diterima | 12 |
| Job sukses | 6 |
| Job failure | 33.33% |
| Queue wait p95 | 541.25 ms |
| Job duration p95 | 60.26 detik |
| Job completion throughput | 0.0533 job/detik |

Urutan beban aktual:

| Tahap | VU | Durasi | Hasil dan analisis |
|---|---:|---:|---|
| Ramp ke normal | 0-2 | 15 detik | Sistem menerima kenaikan beban bertahap. Tidak ada indikasi lonjakan API yang signifikan pada respons keseluruhan. |
| Normal | 2 | 20 detik | API stabil; VU normal menghasilkan aliran submit, polling, dan enqueue tanpa bottleneck utama. |
| Stress | 2-4 | 20 detik | Beban dua kali beban normal mulai mengakumulasi queue. Queue wait tetap rendah dibanding timeout job, dan API masih berada di bawah batas p95. |
| Peak | 4-8 | 15 detik | Beban puncak menimbulkan kontensi pada migration database yang sama. Enqueue tetap terbentuk, tetapi beberapa job gagal urutan atau timeout sehingga completion tidak mencapai seluruh submission. |
| Cooldown | 8-2 | 15 detik | Penurunan beban mulai mengurangi tekanan bersamaan. Tidak ada lonjakan error API baru dalam ringkasan akhir. |
| Recovery | 2-0 | 10 detik | Setelah VU turun ke nol, queue dan worker dapat menyelesaikan/menutup sisa job yang masih terminal dalam grace period; 12 submission tercatat sebagai hasil eksekusi terpisah. |

Analisis:

- p95 API 19.72 ms berada di bawah threshold 500 ms dan error API 2.85% berada di bawah 5%.
- 12 job diterima, tetapi hanya 6 mencapai `success`. Keenam job lain gagal atau timeout
  karena migration pada database yang sama diproses paralel dalam urutan yang tidak berurutan.
- Queue wait p95 adalah 541.25 ms; job duration p95 adalah 60.26 detik. HTTP 202 hanya
  menandakan penerimaan queue dan tidak dihitung sebagai keberhasilan eksekusi migration.
- Threshold utama API terpenuhi. Status job yang gagal tetap dilaporkan sebagai observasi
  aplikasi, bukan digunakan untuk mengubah threshold p95 dan error rate API.
