# Ketentuan Umum

- **Backend** wajib menggunakan bahasa pemrograman **Go**.
- **Frontend** dibebaskan. Tampilan dapat dibuat menggunakan Go template (bukan SPA).
- **Database target** yang dikelola platform dibatasi pada **PostgreSQL**. Dukungan untuk DBMS lain tidak diwajibkan.
- **Automated testing** wajib disediakan untuk setiap fitur backend yang relevan:
  - **Unit test** menguji fungsi atau bagian kecil dari suatu modul secara terisolasi. Dependency di luar bagian yang diuji dapat digantikan dengan mock atau stub.
  - **Integration test** menguji interaksi antara beberapa komponen nyata. Pengujian dapat dilakukan melalui handler API backend, tetapi tidak selalu harus menyalakan HTTP server. Sebagai contoh, test dapat menjalankan database khusus pengujian lalu memanggil handler atau modul backend secara langsung.

Pengujian perlu mencakup alur berhasil, kegagalan, dan penolakan akses sesuai kebutuhan fitur.

# Kebutuhan

```text
Login → Access Control → Select Database → View Schema/Data
                                      → Create Migration → Execute → Commit/Rollback
                                                         → Update Migration History
```

Alur tersebut menunjukkan kemampuan minimum sistem.

## Access Control

Access control adalah teknik otorisasi keamanan yang menentukan sumber daya tertentu yang dapat dilihat atau digunakan oleh pengguna atau sistem dalam suatu infrastruktur TI.

Sebelum mengembangkan fitur yang berkaitan dengan pengelolaan database, penting untuk memastikan bahwa setiap **aksi hanya dapat dilakukan oleh pihak yang memiliki hak akses yang sesuai**. Oleh karena itu, diperlukan mekanisme access control yang diawali dengan identifikasi principal yang melakukan aksi.

Setelah identitas principal diketahui, sistem kemudian memverifikasi apakah principal tersebut memiliki izin untuk menjalankan operasi yang diminta. **Pengaturan terhadap jenis aksi yang dapat dilakukan sebaiknya dipisahkan dari proses identifikasi pengguna dan diletakkan pada tempat khusus** (misalnya dalam satu file atau folder khusus) agar proses pengecekan hak akses, pengelolaan permission, dan audit dapat dilakukan dengan lebih terstruktur.

### Authentication

- Pengguna masuk menggunakan **username dan password**.
- Password harus disimpan menggunakan password hashing yang sesuai, bukan dalam bentuk plaintext.
- Password dan session identifier tidak boleh muncul pada URL, response, maupun log aplikasi.

### Authorization

Setiap tindakan harus diperiksa berdasarkan hak akses pengguna pada backend, bukan hanya disembunyikan dari antarmuka.

### Session Handling

- Login yang berhasil menghasilkan **opaque session identifier** yang acak dan sulit ditebak. Data session disimpan di backend, sedangkan client hanya menerima identifier melalui cookie.
- Cookie session setidaknya menggunakan HttpOnly, SameSite, dan Secure pada environment HTTPS/production.
- Session diperiksa setiap kali pengguna mengakses fitur yang dilindungi dan memiliki:
  - **Idle timeout** — berakhir setelah tidak digunakan dalam jangka waktu tertentu.
  - **Absolute expiration** — berakhir setelah mencapai umur maksimum meskipun pengguna masih aktif.
- Session identifier dirotasi saat login, ketika privilege ditingkatkan, dan secara berkala. Nama cookie tetap, tetapi nilainya diperbarui.
- Identifier lama masih berlaku selama **grace period** singkat untuk menangani response yang tidak diterima atau request yang berjalan bersamaan. Setelah itu, identifier lama tidak berlaku.
- Rotation tidak memperpanjang absolute expiration. Interval rotation, grace period, dan timeout ditentukan serta didokumentasikan oleh tim.
- Logout dan pencabutan akses harus menonaktifkan session di backend. Session yang melewati absolute expiration mengharuskan pengguna login kembali.
- Request yang mengubah state harus memiliki perlindungan CSRF yang sesuai.

### Platform Admin

Platform Admin mengelola keseluruhan platform, meliputi user, group, koneksi database, dan pemberian akses. Platform Admin bukan tingkatan akses terhadap suatu database.

### User, Group, dan Akses Database

- User biasa dapat menjadi anggota satu atau lebih group.
- Akses database diberikan melalui group, bukan langsung kepada masing-masing user.
- Platform Admin menentukan database dan tingkat akses untuk setiap group:
  - **View Only** — hanya dapat melihat schema, data, dan riwayat migration tanpa melakukan perubahan.
  - **View + Edit** — memiliki seluruh akses View Only serta dapat membuat, mengubah, dan menjalankan migration pada database tersebut.
- Satu group dapat memiliki tingkat akses berbeda pada setiap database.
- Jika user tergabung dalam beberapa group yang mengakses database yang sama, tim menentukan dan mendokumentasikan cara menghitung akses efektifnya.

## Database Management

Setelah memastikan bahwa setiap pengguna hanya dapat melakukan aksi sesuai hak akses yang dimilikinya, sistem selanjutnya perlu menyediakan mekanisme untuk **mengelola dan berinteraksi dengan database secara terstruktur**. Database Management mencakup pengelolaan koneksi ke satu atau lebih database, penyediaan informasi mengenai struktur dan isi database, serta penanganan kondisi ketika koneksi gagal atau akses terhadap database tidak diizinkan.

Pemisahan fungsi pengelolaan database dari mekanisme access control membantu menjaga agar proses otorisasi dan proses interaksi dengan database tetap memiliki tanggung jawab yang jelas. Dengan demikian, setiap operasi terhadap database dapat dilakukan melalui mekanisme yang konsisten, mudah dikembangkan, dan tetap mengikuti aturan akses yang telah ditentukan sebelumnya.

- Pengelolaan koneksi ke satu atau lebih database PostgreSQL **oleh platform admin**.
- Database viewer untuk **melihat** schema, table, column, constraint, index, dan data.
- Penanganan kegagalan koneksi dan akses yang tidak sah.

## Migration

Setelah koneksi dan akses terhadap database dapat dikelola, perubahan terhadap struktur maupun data database perlu dilakukan melalui mekanisme yang **terkontrol dan dapat ditelusuri**. **Migration adalah proses untuk menerapkan perubahan pada database secara terstruktur dan berurutan**, misalnya untuk membuat atau mengubah table, column, constraint, index, maupun melakukan perubahan data tertentu. Setiap perubahan direpresentasikan dalam bentuk SQL yang disimpan sebagai bagian dari riwayat migration.

Setiap migration memiliki informasi mengenai versi, SQL yang dijalankan, pengguna yang melakukan perubahan, waktu pelaksanaan, serta status eksekusinya. Migration dijalankan menggunakan transaction sehingga perubahan hanya di-*commit* apabila seluruh proses berhasil dan dapat di-*rollback* ketika terjadi kegagalan. Dengan menyimpan riwayat migration secara linear, kondisi dan perubahan yang pernah terjadi pada setiap database dapat ditinjau kembali dengan lebih mudah.

- Migration query viewer/editor untuk menyusun dan meninjau SQL yang akan dijalankan sebagai migration.
- Pembuatan, perubahan, dan eksekusi migration hanya dapat dilakukan oleh user yang memperoleh akses **View + Edit** melalui group pada database target.
- User dengan akses **View Only** hanya dapat melihat riwayat dan detail migration.
- Riwayat migration yang linear untuk setiap database.
- Pencatatan versi, SQL, pelaksana, waktu, dan status migration.
- Penggunaan transaction untuk melakukan commit ketika berhasil dan rollback ketika gagal.
- Tampilan riwayat perubahan database.

# Spesifikasi BONUS

## Dukungan DBMS tambahan

Implementasi pengelolaan database untuk DBMS selain PostgreSQL, misalnya MySQL atau MariaDB. Fitur utama seperti access control, database viewer, dan migration tetap dapat digunakan pada DBMS tambahan tersebut.

## Schema diff dan migration generation

Sistem dapat membandingkan dua kondisi schema database, misalnya schema saat ini dengan schema yang diinginkan, kemudian menampilkan perbedaannya dan menghasilkan kandidat SQL migration secara otomatis. SQL yang dihasilkan tetap harus dapat ditinjau dan diubah oleh pengguna sebelum dijalankan.

## Query execution plan analysis

Sistem menjalankan dan menampilkan hasil EXPLAIN atau EXPLAIN ANALYZE terhadap query migration sebelum eksekusi untuk membantu pengguna memahami execution plan yang dipilih database. Informasi yang ditampilkan dapat mencakup jenis scan, penggunaan index, join strategy, estimated cost, actual execution time, serta jumlah row yang diproses.

# Pengumpulan, Deliverables, dan Q&A

1. **Pengumpulan:** Setiap kelompok wajib membuat repository GitHub untuk tugas besar. Link repository dikumpulkan melalui form pengumpulan yang akan diinformasikan kemudian (dengan batas mengikuti deadline yang ada di sampul).
2. **Demo:** Setiap kelompok akan mengikuti sesi demo untuk Milestone 1. Jadwal dan mekanisme pelaksanaan demo akan diinformasikan kemudian.
3. **Kelompok:** Pembentukan dan pendaftaran kelompok serta pemilihan Asisten dilakukan melalui Google Sheets (tidak lintas kelas K01, K02, atau K03) selambat-lambatnya **12 Oktober 2026 23:59**; setelah itu akan diacak.
4. **Q&A:** Pertanyaan terkait tugas besar dapat disampaikan melalui Sheets Q&A.
