<?php

declare(strict_types=1);

namespace App\Http\Controllers;

use App\Models\Invoice;
use App\Services\OrdersWebhook;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;

final class InvoiceController
{
    public function __construct(private readonly OrdersWebhook $webhook)
    {
    }

    public function store(Request $request): JsonResponse
    {
        $data = $request->validate([
            'order_id' => ['required', 'integer'],
            'amount' => ['required', 'numeric', 'min:0'],
        ]);
        $invoice = Invoice::create($data + ['status' => 'open']);

        return new JsonResponse($invoice, 201);
    }

    public function show(Invoice $invoice): JsonResponse
    {
        return new JsonResponse($invoice);
    }

    public function pay(Invoice $invoice): JsonResponse
    {
        $invoice->status = 'paid';
        $invoice->save();
        $this->webhook->invoicePaid($invoice);

        return new JsonResponse($invoice);
    }
}
