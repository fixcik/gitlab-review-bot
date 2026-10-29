<?php

declare(strict_types=1);

namespace App\Models;

use Illuminate\Database\Eloquent\Model;

/**
 * @property int $id
 * @property int $order_id
 * @property string $amount
 * @property string $status
 */
final class Invoice extends Model
{
    protected $fillable = ['order_id', 'amount', 'status'];
}
