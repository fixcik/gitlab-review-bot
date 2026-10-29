<?php

declare(strict_types=1);

namespace App\Services;

use App\Models\Invoice;
use GuzzleHttp\Client;
use GuzzleHttp\Exception\GuzzleException;

/**
 * Notifies rb-orders-py that an invoice was paid: POST {ORDERS_HOOK_URL}/hooks/billing
 * with event "invoice.paid". Retries up to three times.
 */
final class OrdersWebhook
{
    private const ATTEMPTS = 3;

    public function __construct(private readonly Client $http)
    {
    }

    public function invoicePaid(Invoice $invoice): void
    {
        $base = rtrim((string) getenv('ORDERS_HOOK_URL'), '/');
        $last = null;
        for ($attempt = 1; $attempt <= self::ATTEMPTS; $attempt++) {
            try {
                $this->http->post($base . '/hooks/billing', [
                    'json' => [
                        'event' => 'invoice.paid',
                        'invoiceId' => $invoice->id,
                        'orderId' => $invoice->order_id,
                        'amount' => $invoice->amount,
                    ],
                    'timeout' => 5,
                ]);

                return;
            } catch (GuzzleException $e) {
                $last = $e;
                usleep(200000 * $attempt);
            }
        }

        throw new \RuntimeException('invoice.paid webhook failed', 0, $last);
    }
}
