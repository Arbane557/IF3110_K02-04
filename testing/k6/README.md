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
job failure rate < 10%
```

Atur threshold lewat `API_P95_THRESHOLD_MS`, `API_ERROR_THRESHOLD`, dan
`JOB_ERROR_THRESHOLD`. HTTP 409 atau 423 yang berasal dari migration yang masih diproses
harus dimasukkan sebagai error untuk beban API dan dilihat apakah disebabkan contention.

## Interpretasi tingkat beban

Catat satu baris untuk setiap tahap: VU, durasi, p50/p95, throughput request, error rate,
queue wait, durasi job, job throughput, dan observasi recovery.

| Tahap | VU | Durasi | API p50/p95 | Throughput API | Error API | Queue p95 | Job p95 | Job throughput | Recovery/observasi |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| Ramp ke normal | 5 | 1m | - | - | - | - | - | - | - |
| Normal | 5 | 2m | - | - | - | - | - | - | - |
| Stress | 10 | 2m | - | - | - | - | - | - | - |
| Peak | 20 | 2m | - | - | - | - | - | - | - |
| Cooldown | 5 | 2m | - | - | - | - | - | - | - |
| Recovery | 0 | 1m | - | - | - | - | - | - | - |

