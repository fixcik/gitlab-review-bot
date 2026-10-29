<?php

declare(strict_types=1);

namespace App;

final class OrderLine
{
    public function __construct(
        public readonly string $sku,
        public readonly int $quantity,
        public readonly float $unitPrice,
    ) {
        if ($quantity <= 0) {
            throw new \InvalidArgumentException('quantity must be positive');
        }
    }
}
