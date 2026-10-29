<?php

declare(strict_types=1);

namespace App\Http\Controllers;

use App\Models\Order;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;

final class OrderController
{
    public function mine(Request $request): JsonResponse
    {
        $orders = Order::query()
            ->where('created_by', $request->user()->email)
            ->orderByDesc('id')
            ->get(['id', 'status', 'total']);

        return new JsonResponse($orders);
    }

    public function store(Request $request): JsonResponse
    {
        $order = Order::create([
            'created_by' => $request->user()->email,
            'status' => 'new',
            'total' => $request->input('total', 0),
        ]);

        return new JsonResponse($order, 201);
    }
}
