<?php

declare(strict_types=1);

namespace App\Tests;

use App\Order;
use App\OrderLine;
use PHPUnit\Framework\TestCase;

final class OrderTest extends TestCase
{
    public function testTotal(): void
    {
        $order = new Order(1, 'a@example.com', [new OrderLine('a', 2, 1.25), new OrderLine('b', 1, 10.0)]);
        self::assertSame(12.5, $order->total());
    }

    public function testRejectsZeroQuantity(): void
    {
        $this->expectException(\InvalidArgumentException::class);
        new OrderLine('a', 0, 1.0);
    }
}
