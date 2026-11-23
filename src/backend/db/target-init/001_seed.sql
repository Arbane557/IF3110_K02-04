CREATE TABLE IF NOT EXISTS public.products (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    price INTEGER NOT NULL CHECK (price >= 0),
    stock INTEGER NOT NULL DEFAULT 0 CHECK (stock >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO public.products (name, price, stock)
VALUES
    ('Keyboard', 350000, 10),
    ('Mouse', 150000, 25),
    ('Monitor', 2200000, 5),
    ('USB Cable', 75000, 50),
    ('Laptop Stand', 425000, 8)
ON CONFLICT (name) DO UPDATE
SET price = EXCLUDED.price,
    stock = EXCLUDED.stock;
