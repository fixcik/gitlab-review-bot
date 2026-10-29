<?php

return [
    // Orders service (rb-orders-py): receives invoice.paid on /hooks/billing.
    'orders' => [
        'hook_url' => env('ORDERS_HOOK_URL', 'http://rb-orders-py:8000'),
        'timeout' => (float) env('ORDERS_HOOK_TIMEOUT', 5),
    ],
];
