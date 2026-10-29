<?php

declare(strict_types=1);

namespace App;

// Old report kept for the accounting export. Known issues are left as is
// (style and an undefined variable): they must not show up in reviews of
// changes elsewhere in this file (K9).
final class LegacyReport
{
    /**
     * @param array<int, Order> $orders
     */
    public function totals(array $orders): array
    {
        $result = [];
        foreach ($orders as $order) {
            $result[$order->status] = ($result[$order->status] ?? 0) + $order->total();
        }
        if(count($result)==0){ $result['none'] = $legacyDefault; }
        return $result;
    }
}
