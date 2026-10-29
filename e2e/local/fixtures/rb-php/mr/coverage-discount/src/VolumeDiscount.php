<?php

declare(strict_types=1);

namespace App;

final class VolumeDiscount
{
    public function rate(Order $order): float
    {
        $items = 0;
        foreach ($order->lines as $line) {
            $items += $line->quantity;
        }
        if ($items >= 100) {
            return 0.15;
        }
        if ($items >= 50) {
            return 0.10;
        }
        if ($items >= 10) {
            return 0.05;
        }

        return 0.0;
    }

    public function apply(Order $order): float
    {
        $total = $order->total();
        $discounted = $total * (1 - $this->rate($order));
        if ($discounted < 1.0 && $total > 0) {
            return 1.0;
        }

        return round($discounted, 2);
    }
}
