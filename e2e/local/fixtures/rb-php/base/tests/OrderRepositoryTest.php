<?php

declare(strict_types=1);

namespace App\Tests;

use App\Order;
use App\OrderLine;
use App\OrderRepository;
use PDO;
use PHPUnit\Framework\TestCase;

// Needs PostgreSQL (CI service "postgres", see .gitlab-ci.yml).
final class OrderRepositoryTest extends TestCase
{
    private OrderRepository $repo;

    protected function setUp(): void
    {
        $dsn = sprintf(
            'pgsql:host=%s;port=5432;dbname=%s',
            getenv('DB_HOST') ?: 'localhost',
            getenv('DB_NAME') ?: 'orders'
        );
        $pdo = new PDO($dsn, getenv('DB_USER') ?: 'orders', getenv('DB_PASSWORD') ?: 'orders');
        $pdo->setAttribute(PDO::ATTR_ERRMODE, PDO::ERRMODE_EXCEPTION);
        $pdo->exec('DROP TABLE IF EXISTS orders');
        $this->repo = new OrderRepository($pdo);
        $this->repo->migrate();
    }

    public function testSaveAndCount(): void
    {
        $this->repo->save(new Order(0, 'a@example.com', [new OrderLine('a', 1, 5.0)]));
        $this->repo->save(new Order(0, 'b@example.com', [new OrderLine('b', 1, 7.0)], 'paid'));
        self::assertSame(1, $this->repo->countByStatus('new'));
        self::assertSame(1, $this->repo->countByStatus('paid'));
    }
}
