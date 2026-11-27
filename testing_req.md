# Testing requirements

Setiap fitur backend baru atau perubahan perilaku yang di-push wajib disertai **unit test dan integration test terkait dalam PR yang sama**. Bug fix wajib punya regression test. Berlaku juga untuk fitur bonus.

## Pola test

- Ikuti package yang sudah ada: `auth`, `database`, `migration`.
- Unit: file `*_test.go`, tanpa database; stub dependency hanya jika diperlukan.
- Integration: file `integration_test.go` dengan tag `integration`, memakai PostgreSQL dan komponen nyata. Gunakan helper fixture yang tersedia supaya data tiap test terpisah dan dibersihkan.
- Gunakan nama test yang menjelaskan perilaku dan urutan setup, aksi, assertion. Tidak perlu framework atau abstraction tambahan.

## Yang wajib diperiksa sesuai fitur

- Alur berhasil: nilai hasil benar dan perubahan benar-benar tersimpan.
- Input salah dan batas relevan: kosong, malformed JSON, ID invalid/tidak ada, duplicate, pagination, panjang input.
- Kegagalan dependency: error tidak berubah menjadi sukses atau response yang menyesatkan.
- Akses: tanpa login, tanpa izin, view vs edit, database lain, serta multi-group bila relevan. Request ditolak tidak boleh mengubah data.
- HTTP: periksa status dan isi response, cookie/redirect jika relevan. Perubahan middleware/route perlu diuji lewat routes asli.
- Auth: password hashing, session expiry/rotation/grace/revoke, cookie flags, CSRF; secret tidak boleh bocor di response atau output test.
- Registry/viewer: credential encryption, CRUD/koneksi, metadata dan isi rows, batas pagination, identifier yang valid/tidak valid.
- Migration: draft create/update, commit, rollback, history actor/waktu/status, execute ulang ditolak. Test rollback harus mengubah data sebelum statement gagal lalu membuktikan perubahan dibatalkan. EXPLAIN tidak boleh mengeksekusi perubahan.
- Concurrency: bila perilaku yang diubah melibatkan lock/cache/request bersamaan, tambahkan case terkait.
- Bug fix: test harus gagal pada bug lama dan lulus setelah perbaikan; jangan ubah expected result untuk mengikuti bug.

## Sebelum push

Jalankan unit, integration, dan vet. Semua harus lulus. Test harus independen, bisa diulang, tidak menghapus data bersama, dan tidak bergantung pada sleep atau urutan rows tanpa ORDER BY. Integration yang dipilih tetapi URL database belum diatur harus gagal, bukan skip diam-diam.

Cantumkan perubahan perilaku dan test terkait di PR. Reviewer memeriksa apakah assertion benar-benar membuktikan perilakunya; coverage tinggi atau CI hijau saja tidak cukup. Tidak perlu menambah test yang hanya menyalin implementation detail.

Perintah dan catatan pengembangan ada di [test.md](test.md). CI menjalankan suite pada push/PR; kewajiban update test fitur tetap diperiksa saat review.
