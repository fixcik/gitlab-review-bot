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

    #[Route('/orders/mine/{email}', methods: ['GET'])]
    public function mine(string $email): JsonResponse
    {
        $out = [];
        foreach ($this->orders->findByAuthor($email) as $order) {
            $out[] = [
                'id' => $order->getId(),
                'status' => $order->getStatus(),
                'created_by' => $order->getCreatedBy(),
                'total' => $order->getTotal(),
            ];
        }

        return new JsonResponse($out);
    }
}
