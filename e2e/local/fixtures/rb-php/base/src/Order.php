<?php

declare(strict_types=1);

namespace App;

final class Order
{
    /**
     * @param array<int, OrderLine> $lines
     */
    public function __construct(
        public readonly int $id,
        public readonly string $customerEmail,
        public readonly array $lines,
        public readonly string $status = 'new',
    ) {
    }

    public function total(): float
    {
        $sum = 0.0;
        foreach ($this->lines as $line) {
            $sum += $line->quantity * $line->unitPrice;
        }

        return round($sum, 2);
    }
}
