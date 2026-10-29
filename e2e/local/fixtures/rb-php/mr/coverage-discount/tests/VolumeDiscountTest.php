<?php

declare(strict_types=1);

namespace App\Tests;

use App\Order;
use App\OrderLine;
use App\VolumeDiscount;
use PHPUnit\Framework\TestCase;

final class VolumeDiscountTest extends TestCase
{
    public function testSmallOrderHasNoDiscount(): void
    {
        $order = new Order(1, 'a@example.com', [new OrderLine('a', 2, 10.0)]);
        self::assertSame(0.0, (new VolumeDiscount())->rate($order));
    }

    public function testTenItems(): void
    {
        $order = new Order(1, 'a@example.com', [new OrderLine('a', 10, 10.0)]);
        self::assertSame(95.0, (new VolumeDiscount())->apply($order));
    }
}
