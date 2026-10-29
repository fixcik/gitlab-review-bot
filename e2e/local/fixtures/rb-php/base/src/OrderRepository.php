<?php

declare(strict_types=1);

namespace App;

use PDO;

final class OrderRepository
{
    public function __construct(private readonly PDO $pdo)
    {
    }

    public function migrate(): void
    {
        $this->pdo->exec(
            'CREATE TABLE IF NOT EXISTS orders (
                id SERIAL PRIMARY KEY,
                customer_email TEXT NOT NULL,
                status TEXT NOT NULL DEFAULT \'new\',
                total NUMERIC(12, 2) NOT NULL DEFAULT 0
            )'
        );
    }

    public function save(Order $order): int
    {
        $stmt = $this->pdo->prepare(
            'INSERT INTO orders (customer_email, status, total) VALUES (:email, :status, :total) RETURNING id'
        );
        $stmt->execute([
            'email' => $order->customerEmail,
            'status' => $order->status,
            'total' => $order->total(),
        ]);

        return (int) $stmt->fetchColumn();
    }

    public function countByStatus(string $status): int
    {
        $stmt = $this->pdo->prepare('SELECT count(*) FROM orders WHERE status = :status');
        $stmt->execute(['status' => $status]);

        return (int) $stmt->fetchColumn();
    }
}
