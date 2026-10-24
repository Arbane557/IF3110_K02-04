-- Account testing.
INSERT INTO users (username, password_hash, is_admin)
VALUES
    ('admin', '$2b$12$LEJT7ZaXZK/R9x3f5QcEseHDevK7uybs7Fu9LNGS/hNu748G6G2Wy', TRUE),
    ('viewer', '$2b$12$roOHoJ4ItabuH5mwx1caM.VFGvZfBcT53tNOE/J5kc.li3gN8VuHO', FALSE),
    ('editor', '$2b$12$QcDTlQNTugswDcZfchbARemzyOcOg.pDvk61TDiKeC16JewBzEH1e', FALSE)
ON CONFLICT (username) DO NOTHING;
