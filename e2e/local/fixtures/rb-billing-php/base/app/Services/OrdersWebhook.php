<?php

declare(strict_types=1);

namespace App\Services;

use App\Models\Invoice;
use GuzzleHttp\Client;

/**
 * Notifies rb-orders-py that an invoice was paid: POST {ORDERS_HOOK_URL}/hooks/billing
 * with event "invoice.paid".
 */
final class OrdersWebhook
{
    public function __construct(private readonly Client $http)
    {
    }

    public function invoicePaid(Invoice $invoice): void
    {
        $base = rtrim((string) getenv('ORDERS_HOOK_URL'), '/');
        $this->http->post($base . '/hooks/billing', [
            'json' => [
                'event' => 'invoice.paid',
                'invoice_id' => $invoice->id,
                'order_id' => $invoice->order_id,
                'amount' => $invoice->amount,
            ],
            'timeout' => 5,
        ]);
    }
}
