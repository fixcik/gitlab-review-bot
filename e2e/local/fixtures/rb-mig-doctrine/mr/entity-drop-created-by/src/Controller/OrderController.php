<?php

declare(strict_types=1);

namespace App\Controller;

use App\Repository\OrderRepository;
use Symfony\Component\HttpFoundation\JsonResponse;
use Symfony\Component\Routing\Attribute\Route;

final class OrderController
{
    public function __construct(private readonly OrderRepository $orders)
    {
    }

    #[Route('/orders/status/{status}', methods: ['GET'])]
    public function byStatus(string $status): JsonResponse
    {
        $out = [];
        foreach ($this->orders->findByStatus($status) as $order) {
            $out[] = [
                'id' => $order->getId(),
                'status' => $order->getStatus(),
                'total' => $order->getTotal(),
            ];
        }

        return new JsonResponse($out);
    }
}
